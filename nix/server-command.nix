{ lib, instance }:
let
  quoteExec =
    arg:
    "\""
    + lib.replaceStrings [ "\\" "\"" "%" "$" "\n" "\r" ] [ "\\\\" "\\\"" "%%" "$$" "\\n" "\\r" ] arg
    + "\"";
in
lib.concatMapStringsSep " " quoteExec (
  [
    (lib.getExe instance.package)
    "serve"
    "--network"
    instance.network
    "--db"
    instance.database
    "--listen"
    instance.listen
  ]
  ++ lib.optionals (instance.storageKeyPassEntry != null) [
    "--storage-key-pass-entry"
    instance.storageKeyPassEntry
  ]
  ++ lib.optionals (instance.storageKeyFile != null) [
    "--storage-key-file"
    (toString instance.storageKeyFile)
  ]
  ++ lib.optionals (instance.apiTokenPassEntry != null) [
    "--api-token-pass-entry"
    instance.apiTokenPassEntry
  ]
  ++ lib.optionals (instance.apiTokenFile != null) [
    "--api-token-file"
    (toString instance.apiTokenFile)
  ]
)
