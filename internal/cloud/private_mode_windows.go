//go:build windows

package cloud

import "os"

// Windows does not expose ACL confidentiality through os.FileMode. Phase 4
// production cloud execution is Linux-only until a dedicated ACL check exists.
func privateFileModeError(os.FileInfo) error { return nil }
