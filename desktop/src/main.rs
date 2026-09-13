#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::fs;
use std::path::PathBuf;
use std::sync::Mutex;

use tauri::menu::{Menu, MenuItem};
use tauri::tray::{TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Manager, State, WindowEvent};
use tauri_plugin_notification::NotificationExt;

const KEYRING_SERVICE: &str = "google-messages-multidevice-bridge";
const KEYRING_USER: &str = "api-token";

// Preconfiguration hooks for Nix-managed hosts. The secret itself is never
// baked into the binary: the wrapper (or the user's session environment)
// supplies it at launch, and the keyring/file store stays the fallback.
const ENV_BRIDGE_URL: &str = "GOOGLE_MESSAGES_BRIDGE_URL";
const ENV_TOKEN: &str = "GOOGLE_MESSAGES_BRIDGE_TOKEN";
const ENV_TOKEN_FILE: &str = "GOOGLE_MESSAGES_BRIDGE_TOKEN_FILE";

struct Paths {
    config_dir: PathBuf,
}

impl Paths {
    fn url_file(&self) -> PathBuf {
        self.config_dir.join("bridge-url")
    }
    fn token_file(&self) -> PathBuf {
        self.config_dir.join("token")
    }
}

struct Unread(Mutex<u32>);

fn keyring_entry() -> Result<keyring::Entry, keyring::Error> {
    keyring::Entry::new(KEYRING_SERVICE, KEYRING_USER)
}

fn read_bridge_url(paths: &Paths) -> Option<String> {
    // A managed URL wins so every host opens the same bridge; it is validated
    // the same way as a manually entered one.
    if let Some(url) = env_value(ENV_BRIDGE_URL).and_then(|raw| validate_bridge_url(&raw)) {
        return Some(url);
    }
    let value = fs::read_to_string(paths.url_file()).ok()?;
    nonempty(value)
}

/// Trimmed non-blank text, or `None` for missing/blank input.
fn nonempty(value: String) -> Option<String> {
    let trimmed = value.trim();
    (!trimmed.is_empty()).then(|| trimmed.to_string())
}

fn env_value(name: &str) -> Option<String> {
    std::env::var(name).ok().and_then(nonempty)
}

fn env_file_value(name: &str) -> Option<String> {
    let path = std::env::var(name).ok()?;
    let path = path.trim();
    if path.is_empty() {
        return None;
    }
    fs::read_to_string(path).ok().and_then(nonempty)
}

fn validate_bridge_url(raw: &str) -> Option<String> {
    let parsed = url::Url::parse(raw.trim()).ok()?;
    if !matches!(parsed.scheme(), "http" | "https") {
        return None;
    }
    Some(parsed.to_string())
}

/// First hit wins: explicit env token, token file, then whatever is stored.
/// The stored lookup stays lazy so a preseeded token never touches the keyring.
fn select_token(
    direct: Option<String>,
    from_file: Option<String>,
    stored: impl FnOnce() -> Option<String>,
) -> Option<String> {
    direct.or(from_file).or_else(stored)
}

fn write_private(path: &PathBuf, contents: &str) -> Result<(), String> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).map_err(|e| e.to_string())?;
    }
    fs::write(path, contents).map_err(|e| e.to_string())?;
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(path, fs::Permissions::from_mode(0o600)).map_err(|e| e.to_string())?;
    }
    Ok(())
}

#[tauri::command]
fn get_bridge_url(paths: State<Paths>) -> Option<String> {
    read_bridge_url(&paths)
}

#[tauri::command]
fn save_bridge_url(app: AppHandle, paths: State<Paths>, url: String) -> Result<(), String> {
    let normalized = validate_bridge_url(&url).ok_or("Use an http or https URL.")?;
    write_private(&paths.url_file(), &normalized)?;
    navigate(&app, &normalized)
}

fn navigate(app: &AppHandle, url: &str) -> Result<(), String> {
    let window = app.get_webview_window("main").ok_or("main window missing")?;
    let target = url::Url::parse(url).map_err(|e| e.to_string())?;
    window.navigate(target).map_err(|e| e.to_string())
}

#[tauri::command]
fn open_setup(app: AppHandle) -> Result<(), String> {
    let window = app.get_webview_window("main").ok_or("main window missing")?;
    let target = url::Url::parse("tauri://localhost/index.html").map_err(|e| e.to_string())?;
    window.navigate(target).map_err(|e| e.to_string())
}

// The OS keyring is preferred; a private file in the config directory covers
// sessions without a secret service. A preseeded token from the environment
// wins over both so managed hosts unlock without typing anything.
#[tauri::command]
fn get_token(paths: State<Paths>) -> Option<String> {
    select_token(
        env_value(ENV_TOKEN),
        env_file_value(ENV_TOKEN_FILE),
        || stored_token(&paths),
    )
}

fn stored_token(paths: &Paths) -> Option<String> {
    if let Ok(entry) = keyring_entry() {
        if let Ok(token) = entry.get_password() {
            return Some(token);
        }
    }
    let token = fs::read_to_string(paths.token_file()).ok()?;
    nonempty(token)
}

#[tauri::command]
fn save_token(paths: State<Paths>, token: String) -> Result<(), String> {
    if let Ok(entry) = keyring_entry() {
        if entry.set_password(&token).is_ok() {
            let _ = fs::remove_file(paths.token_file());
            return Ok(());
        }
    }
    write_private(&paths.token_file(), &token)
}

#[tauri::command]
fn clear_token(paths: State<Paths>) {
    if let Ok(entry) = keyring_entry() {
        let _ = entry.delete_credential();
    }
    let _ = fs::remove_file(paths.token_file());
    // Locking forgets the preseed for this run too; it returns on next launch.
    std::env::remove_var(ENV_TOKEN);
    std::env::remove_var(ENV_TOKEN_FILE);
}

#[tauri::command]
fn set_unread(app: AppHandle, unread: State<Unread>, count: u32) {
    *unread.0.lock().unwrap() = count;
    if let Some(window) = app.get_webview_window("main") {
        let title = if count > 0 {
            format!("Messages ({count})")
        } else {
            "Messages".to_string()
        };
        let _ = window.set_title(&title);
        let _ = window.set_badge_count(if count > 0 { Some(count as i64) } else { None });
    }
    if let Some(tray) = app.tray_by_id("main") {
        let _ = tray.set_tooltip(Some(if count > 0 {
            format!("Messages · {count} unread")
        } else {
            "Messages".to_string()
        }));
    }
}

#[tauri::command]
fn notify(app: AppHandle, title: String, body: String) -> Result<(), String> {
    app.notification()
        .builder()
        .title(title)
        .body(body)
        .show()
        .map_err(|e| e.to_string())
}

fn show_main(app: &AppHandle) {
    if let Some(window) = app.get_webview_window("main") {
        let _ = window.show();
        let _ = window.unminimize();
        let _ = window.set_focus();
    }
}

fn main() {
    tauri::Builder::default()
        .plugin(tauri_plugin_notification::init())
        .manage(Unread(Mutex::new(0)))
        .setup(|app| {
            let config_dir = app.path().app_config_dir()?;
            let paths = Paths { config_dir };
            let bridge_url = read_bridge_url(&paths);
            app.manage(paths);

            let open = MenuItem::with_id(app, "open", "Open Messages", true, None::<&str>)?;
            let setup = MenuItem::with_id(app, "setup", "Change bridge URL…", true, None::<&str>)?;
            let quit = MenuItem::with_id(app, "quit", "Quit", true, None::<&str>)?;
            let menu = Menu::with_items(app, &[&open, &setup, &quit])?;
            TrayIconBuilder::with_id("main")
                .icon(tauri::image::Image::from_bytes(include_bytes!("../icons/icon-128.png"))?)
                .tooltip("Messages")
                .menu(&menu)
                .show_menu_on_left_click(false)
                .on_menu_event(|app, event| match event.id.as_ref() {
                    "open" => show_main(app),
                    "setup" => {
                        show_main(app);
                        let _ = open_setup(app.clone());
                    }
                    "quit" => app.exit(0),
                    _ => {}
                })
                .on_tray_icon_event(|tray, event| {
                    if let TrayIconEvent::Click { .. } = event {
                        show_main(tray.app_handle());
                    }
                })
                .build(app)?;

            if let Some(url) = bridge_url {
                let _ = navigate(app.handle(), &url);
            }
            Ok(())
        })
        .on_window_event(|window, event| {
            if let WindowEvent::CloseRequested { api, .. } = event {
                let _ = window.hide();
                api.prevent_close();
            }
        })
        .invoke_handler(tauri::generate_handler![
            get_bridge_url,
            save_bridge_url,
            open_setup,
            get_token,
            save_token,
            clear_token,
            set_unread,
            notify
        ])
        .run(tauri::generate_context!())
        .expect("error while running Messages");
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn nonempty_trims_and_rejects_blank() {
        assert_eq!(nonempty("  abc\n".to_string()), Some("abc".to_string()));
        assert_eq!(nonempty("   ".to_string()), None);
        assert_eq!(nonempty(String::new()), None);
    }

    #[test]
    fn validate_bridge_url_accepts_http_and_https() {
        let https = validate_bridge_url("https://bridge.example.ts.net:8443").unwrap();
        assert!(https.starts_with("https://bridge.example.ts.net:8443"));
        assert!(validate_bridge_url("http://127.0.0.1:8787").is_some());
    }

    #[test]
    fn validate_bridge_url_rejects_other_schemes_and_garbage() {
        assert!(validate_bridge_url("ftp://example.com").is_none());
        assert!(validate_bridge_url("not a url").is_none());
        assert!(validate_bridge_url("").is_none());
        assert!(validate_bridge_url("   ").is_none());
    }

    #[test]
    fn select_token_prefers_direct_env_then_file_then_stored() {
        let stored = || Some("stored".to_string());
        assert_eq!(
            select_token(Some("direct".into()), Some("file".into()), stored),
            Some("direct".to_string())
        );
        assert_eq!(
            select_token(None, Some("file".into()), stored),
            Some("file".to_string())
        );
        assert_eq!(select_token(None, None, stored), Some("stored".to_string()));
        let none = || None;
        assert_eq!(select_token(None, None, none), None);
    }

    #[test]
    fn select_token_leaves_stored_lookup_lazy() {
        let stored = || panic!("stored lookup must not run when env provides a token");
        assert_eq!(
            select_token(Some("direct".into()), None, stored),
            Some("direct".to_string())
        );
    }
}
