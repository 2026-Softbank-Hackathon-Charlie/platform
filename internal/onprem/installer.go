package onprem

import _ "embed"

// Installer claims a one-use grant, installs the local connector and waits
// for the server's independently observed registration checks to pass.
//
//go:embed installer.sh
var Installer string
