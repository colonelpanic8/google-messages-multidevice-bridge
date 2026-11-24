use tauri::ipc::Response;
use tauri::AppHandle;

#[cfg(target_os = "linux")]
use crate::MAX_ATTACHMENT_BYTES;

#[tauri::command]
pub async fn read_clipboard_image(app: AppHandle) -> Result<Response, String> {
    #[cfg(target_os = "linux")]
    {
        let (sender, mut receiver) = tauri::async_runtime::channel(1);
        app.run_on_main_thread(move || {
            let clipboard = gtk::Clipboard::get(&gtk::gdk::SELECTION_CLIPBOARD);
            let png = gtk::gdk::Atom::intern("image/png");
            clipboard.request_contents(&png, move |clipboard, data| {
                if data.length() > 0 {
                    let image = if data.length() as usize > MAX_ATTACHMENT_BYTES {
                        Err(IMAGE_TOO_LARGE.to_string())
                    } else {
                        Ok(data.data())
                    };
                    let _ = sender.try_send(image);
                    return;
                }
                clipboard.request_image(move |_, pixbuf| {
                    let image = match pixbuf {
                        Some(pixbuf) => pixbuf
                            .save_to_bufferv("png", &[])
                            .map_err(|error| error.to_string())
                            .and_then(checked_image),
                        None => Ok(Vec::new()),
                    };
                    let _ = sender.try_send(image);
                });
            });
        })
        .map_err(|error| error.to_string())?;
        let image = receiver
            .recv()
            .await
            .ok_or("Clipboard read interrupted.")??;
        Ok(Response::new(image))
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = app;
        Ok(Response::new(tauri::ipc::InvokeResponseBody::Json(
            "null".into(),
        )))
    }
}

#[cfg(target_os = "linux")]
const IMAGE_TOO_LARGE: &str = "Clipboard images must be 20 MiB or less.";

#[cfg(target_os = "linux")]
fn checked_image(bytes: Vec<u8>) -> Result<Vec<u8>, String> {
    if bytes.len() > MAX_ATTACHMENT_BYTES {
        return Err(IMAGE_TOO_LARGE.into());
    }
    Ok(bytes)
}

#[cfg(target_os = "linux")]
#[tauri::command]
pub async fn write_clipboard_image(
    app: AppHandle,
    request: tauri::ipc::Request<'_>,
) -> Result<(), String> {
    let tauri::ipc::InvokeBody::Raw(bytes) = request.body() else {
        return Err("Expected image bytes.".into());
    };
    if bytes.len() > MAX_ATTACHMENT_BYTES {
        return Err(IMAGE_TOO_LARGE.into());
    }
    let bytes = bytes.clone();
    let (sender, mut receiver) = tauri::async_runtime::channel(1);
    app.run_on_main_thread(move || {
        use gtk::{gdk, gdk_pixbuf, gio, glib};

        let stream = gio::MemoryInputStream::from_bytes(&glib::Bytes::from_owned(bytes));
        let image = gdk_pixbuf::Pixbuf::from_stream(&stream, None::<&gio::Cancellable>);
        let result = image
            .map(|image| gtk::Clipboard::get(&gdk::SELECTION_CLIPBOARD).set_image(&image))
            .map_err(|error| error.to_string());
        let _ = sender.try_send(result);
    })
    .map_err(|error| error.to_string())?;
    receiver
        .recv()
        .await
        .ok_or("Clipboard write interrupted.")?
}
