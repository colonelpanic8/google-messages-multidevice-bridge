{
  lib,
  rustPlatform,
  pkg-config,
  wrapGAppsHook3,
  webkitgtk_4_1,
  gtk3,
  libsoup_3,
  openssl,
  dbus,
  libayatana-appindicator,
  glib-networking,
}:

rustPlatform.buildRustPackage {
  pname = "google-messages-desktop";
  version = "0.1.0";
  src = ../desktop;
  cargoLock.lockFile = ../desktop/Cargo.lock;

  nativeBuildInputs = [
    pkg-config
    wrapGAppsHook3
  ];
  buildInputs = [
    webkitgtk_4_1
    gtk3
    libsoup_3
    openssl
    dbus
    libayatana-appindicator
    glib-networking
  ];

  postInstall = ''
    install -Dm644 icons/icon.png $out/share/icons/hicolor/512x512/apps/google-messages-desktop.png
    install -Dm644 icons/icon-128.png $out/share/icons/hicolor/128x128/apps/google-messages-desktop.png
    mkdir -p $out/share/applications
    cat > $out/share/applications/google-messages-desktop.desktop <<DESKTOP
    [Desktop Entry]
    Type=Application
    Name=Messages
    Comment=Google Messages through the multi-device bridge
    Exec=google-messages-desktop
    Icon=google-messages-desktop
    Categories=Network;InstantMessaging;
    StartupWMClass=google-messages-desktop
    DESKTOP
  '';

  meta = {
    description = "Desktop window for the Google Messages Multi-Device Bridge";
    license = lib.licenses.agpl3Plus;
    mainProgram = "google-messages-desktop";
    platforms = lib.platforms.linux;
  };
}
