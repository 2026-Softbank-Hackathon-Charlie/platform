package onprem

import _ "embed"

// Installer is served by this API, never synthesized as a command to a
// nonexistent script. It claims a grant; it does not register a cluster.
//
//go:embed installer.sh
var Installer string
