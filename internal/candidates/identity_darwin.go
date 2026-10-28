//go:build darwin

package candidates

import "golang.org/x/sys/unix"

func candidateIdentityFromStat(stat *unix.Stat_t) candidateFileIdentity {
	return candidateFileIdentity{
		dev:         uint64(stat.Dev),
		ino:         uint64(stat.Ino),
		mode:        uint32(stat.Mode),
		size:        stat.Size,
		mtimeSecond: stat.Mtim.Sec,
		mtimeNano:   stat.Mtim.Nsec,
		ctimeSecond: stat.Ctim.Sec,
		ctimeNano:   stat.Ctim.Nsec,
	}
}
