fn main() {
    println!("cargo:rerun-if-env-changed=GMESSAGES_DESKTOP_COMMIT");
    tauri_build::build()
}
