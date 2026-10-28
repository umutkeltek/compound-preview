package home

// These seams belong only to streamed restore's unpublished staging transaction.
// Other writers and legacy restore retain their existing durability protocol.
var (
	restoreStreamedFileSync          = syncStreamedRestoreFile
	restoreStreamedDeviceBarrier     = syncStreamedRestoreBarrier
	restoreStreamedPublishParentSync = syncStreamedRestoreParent
)
