use std::fs::{self, File};
use std::io::{self, Read};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use tauri::ipc::Response;
use tauri::State;

use crate::MAX_ATTACHMENT_BYTES;

#[derive(Default)]
pub struct DroppedFiles(Mutex<Vec<PathBuf>>);

impl DroppedFiles {
    pub fn remember(&self, paths: &[PathBuf]) {
        *self.0.lock().unwrap() = paths.iter().take(10).cloned().collect();
    }

    fn take(&self, requested: &[PathBuf]) -> Option<Vec<PathBuf>> {
        let mut allowed = self.0.lock().unwrap();
        (allowed.as_slice() == requested).then(|| std::mem::take(&mut *allowed))
    }
}

fn read_file(path: &Path, limit: usize) -> io::Result<Vec<u8>> {
    if !path.is_absolute() || !fs::metadata(path)?.is_file() {
        return Err(io::Error::other("Drop regular files to attach them."));
    }
    let file = File::open(path)?;
    let mut bytes = Vec::new();
    file.take(limit as u64 + 1).read_to_end(&mut bytes)?;
    if bytes.len() > limit {
        return Err(io::Error::other("Attachments must total 20 MiB or less."));
    }
    if bytes.is_empty() {
        return Err(io::Error::other("An empty file cannot be attached."));
    }
    Ok(bytes)
}

fn read_files(paths: &[PathBuf], mut remaining: usize) -> io::Result<Vec<u8>> {
    let mut output = Vec::new();
    for path in paths {
        let bytes = read_file(path, remaining)?;
        #[cfg(target_os = "linux")]
        let mime = gtk::gio::content_type_get_mime_type(
            &gtk::gio::content_type_guess(Some(path), &bytes[..bytes.len().min(512)]).0,
        );
        #[cfg(not(target_os = "linux"))]
        let mime: Option<String> = None;
        let mime = mime.as_deref().unwrap_or("application/octet-stream");

        // Each file is a little-endian byte count, a NUL-terminated MIME type,
        // and the original bytes. The invoke response carries the batch as binary.
        output.extend_from_slice(&(bytes.len() as u32).to_le_bytes());
        output.extend_from_slice(mime.as_bytes());
        output.push(0);
        output.extend_from_slice(&bytes);
        remaining -= bytes.len();
    }
    Ok(output)
}

#[tauri::command]
pub async fn read_dropped_files(
    state: State<'_, DroppedFiles>,
    paths: Vec<PathBuf>,
    limit: usize,
) -> Result<Response, String> {
    let paths = state
        .take(&paths)
        .ok_or("Drop the files again to attach them.")?;
    tauri::async_runtime::spawn_blocking(move || {
        read_files(&paths, limit.min(MAX_ATTACHMENT_BYTES))
            .map(Response::new)
            .map_err(|error| error.to_string())
    })
    .await
    .map_err(|error| error.to_string())?
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_the_current_drop_can_be_read_once() {
        let dropped = DroppedFiles::default();
        let first = [PathBuf::from("/first.png")];
        let second = [PathBuf::from("/second.png"), PathBuf::from("/third.png")];

        dropped.remember(&first);
        dropped.remember(&second);
        assert_eq!(dropped.take(&first), None);
        assert_eq!(dropped.take(&second[..1]), None);
        assert_eq!(dropped.take(&second), Some(second.to_vec()));
        assert_eq!(dropped.take(&second), None);
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn drop_preserves_the_image_type_and_bytes_within_the_budget() {
        let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("../client/icons/favicon-32.png");
        let original = fs::read(&path).unwrap();
        let mut expected = (original.len() as u32).to_le_bytes().to_vec();
        expected.extend_from_slice(b"image/png\0");
        expected.extend_from_slice(&original);

        assert!(read_files(std::slice::from_ref(&path), original.len() - 1).is_err());
        assert_eq!(read_files(&[path], original.len()).unwrap(), expected);
    }
}
