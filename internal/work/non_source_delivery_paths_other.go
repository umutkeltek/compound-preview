//go:build !unix

package work

import "os"

// Private runtime delivery needs native link-count evidence. Unsupported
// platforms fail closed, rather than interpreting missing evidence as safe.
func privateDeliverySingleLink(info os.FileInfo) bool { return false }
