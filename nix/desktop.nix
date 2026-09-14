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
  # The desktop app bundles the client tree, so both have to be in scope even
  # though the crate itself lives in ./desktop.
  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../desktop
      ../client
    ];
  };
  sourceRoot = "source/desktop";
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
    install -Dm644 ../client/favicon.svg $out/share/icons/hicolor/scalable/apps/google-messages-desktop.svg
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

  # The tray-icon stack loads libayatana-appindicator3 via dlopen, which Nix's
  # RPATH does not cover, so the library must be discoverable at runtime.
  # WEBKIT_DISABLE_DMABUF_RENDERER avoids the explicit-sync dmabuf path that
  # kills WebKitGTK surfaces on NVIDIA ("Missing acquire timeline").
  postFixup = ''
    wrapProgram $out/bin/google-messages-desktop \
      --prefix LD_LIBRARY_PATH : "${lib.makeLibraryPath [ libayatana-appindicator ]}" \
      --set-default WEBKIT_DISABLE_DMABUF_RENDERER 1
  '';

  meta = {
    description = "Desktop window for the Google Messages Multi-Device Bridge";
    license = lib.licenses.agpl3Plus;
    mainProgram = "google-messages-desktop";
    platforms = lib.platforms.linux;
  };
}
