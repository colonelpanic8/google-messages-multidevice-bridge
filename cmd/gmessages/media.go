package main

import (
	"bytes"
	"context"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/client"
	"github.com/colonelpanic8/google-messages-multidevice-bridge/internal/model"
)

var attachmentIDPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func checkAttachmentID(id string) error {
	if !attachmentIDPattern.MatchString(id) {
		return fmt.Errorf("invalid attachment ID %q: use the id= shown with a message's attachment", id)
	}
	return nil
}

// sniffMIME identifies attachment bytes, which the bridge serves as
// application/octet-stream. It covers the formats phones send that
// http.DetectContentType does not.
func sniffMIME(data []byte) string {
	if len(data) >= 12 && string(data[4:8]) == "ftyp" {
		switch brand := string(data[8:12]); {
		case brand == "heic" || brand == "heix" || brand == "heim" || brand == "heis":
			return "image/heic"
		case brand == "mif1" || brand == "msf1":
			return "image/heif"
		case brand == "avif" || brand == "avis":
			return "image/avif"
		case brand == "M4A " || brand == "M4B ":
			return "audio/mp4"
		case strings.HasPrefix(brand, "3gp") || strings.HasPrefix(brand, "3gg"):
			return "video/3gpp"
		case brand == "qt  ":
			return "video/quicktime"
		}
	}
	head := bytes.TrimLeft(data[:min(len(data), 64)], "\xef\xbb\xbf \t\r\n")
	switch {
	case bytes.HasPrefix(data, []byte("#!AMR")):
		return "audio/amr"
	case bytes.HasPrefix(bytes.ToUpper(head), []byte("BEGIN:VCARD")):
		return "text/vcard"
	}
	kind, _, _ := strings.Cut(http.DetectContentType(data), ";")
	return kind
}

var extensions = map[string]string{
	"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
	"image/bmp": ".bmp", "image/heic": ".heic", "image/heif": ".heif", "image/avif": ".avif",
	"video/mp4": ".mp4", "video/webm": ".webm", "video/3gpp": ".3gp", "video/quicktime": ".mov",
	"audio/mpeg": ".mp3", "audio/mp4": ".m4a", "audio/ogg": ".ogg", "audio/amr": ".amr",
	"audio/wave": ".wav", "application/pdf": ".pdf", "text/plain": ".txt", "text/vcard": ".vcf",
	"application/zip": ".zip",
}

func extension(kind string) string {
	if ext, ok := extensions[kind]; ok {
		return ext
	}
	if exts, _ := mime.ExtensionsByType(kind); len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// attachmentDir prefers the per-user runtime directory, which is private and
// cleared at logout, so downloaded media does not accumulate in plaintext.
func attachmentDir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		var err error
		if base, err = os.UserCacheDir(); err != nil {
			return "", err
		}
	}
	dir := filepath.Join(base, "gmessages", "attachments")
	return dir, os.MkdirAll(dir, 0o700)
}

type savedAttachment struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	MIME string `json:"mime"`
	Size int    `json:"size"`
}

// saveAttachment downloads an attachment into dest, which names a file, an
// existing directory, or nothing for the default attachment directory.
func saveAttachment(ctx context.Context, c *client.Client, id, dest string) (savedAttachment, []byte, error) {
	if err := checkAttachmentID(id); err != nil {
		return savedAttachment{}, nil, err
	}
	data, err := c.Attachment(ctx, id)
	if err != nil {
		return savedAttachment{}, nil, err
	}
	s := savedAttachment{ID: id, MIME: sniffMIME(data), Size: len(data)}
	name := id[:16] + extension(s.MIME)
	if dest == "" {
		if dest, err = attachmentDir(); err != nil {
			return s, nil, err
		}
	}
	if info, err := os.Stat(dest); err == nil && info.IsDir() {
		dest = filepath.Join(dest, name)
	}
	if s.Path, err = filepath.Abs(dest); err != nil {
		return s, nil, err
	}
	return s, data, os.WriteFile(s.Path, data, 0o600)
}

func formatSaved(s savedAttachment) string {
	return fmt.Sprintf("%s  %s %s", s.Path, s.MIME, formatSize(int64(s.Size)))
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

func formatAttachment(a model.Attachment) string {
	var b strings.Builder
	fmt.Fprintf(&b, " [attachment %s", cmpOr(a.Name, "(unnamed)"))
	if a.MIME != "" {
		b.WriteString(" " + a.MIME)
	}
	if a.Size > 0 {
		b.WriteString(" " + formatSize(a.Size))
	}
	switch {
	case !a.Available:
		b.WriteString(" unavailable")
	case a.Preview:
		b.WriteString(" thumbnail-only")
	}
	if a.Requestable {
		b.WriteString(" full-requestable")
	}
	fmt.Fprintf(&b, " id=%s]", a.ID)
	return b.String()
}
