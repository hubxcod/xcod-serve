// Package deploy holds the installation files that xcod serves itself (the primary serves the
// installer to repeaters).
package deploy

import _ "embed"

//go:embed install.sh
var InstallScript []byte
