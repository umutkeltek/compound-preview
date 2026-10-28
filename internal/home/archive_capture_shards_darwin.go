//go:build darwin

package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

const captureBodyShardCount = 256

var captureOpenShard = (*os.Root).OpenRoot

type archiveCaptureShard struct {
	info fs.FileInfo
	root *os.Root
	dir  *os.File
}
type archiveCaptureShards struct {
	stage  *os.Root
	shards [captureBodyShardCount]archiveCaptureShard
}

func captureShardName(n int) string { return fmt.Sprintf("bodies-%03d", n) }
func captureBodyLeaf(n int) string  { return fmt.Sprintf("body-%09d", n) }
func captureBodyName(n int) string {
	return captureShardName((n-1)%captureBodyShardCount) + "/" + captureBodyLeaf(n)
}

func newArchiveCaptureShards(stage *os.Root) (*archiveCaptureShards, error) {
	return &archiveCaptureShards{stage: stage}, nil
}

// Open only shards needed by this capture; tiny homes do not pay the maximum
// descriptor/directory cost. Every opened shard remains pinned until cleanup.
func (s *archiveCaptureShards) ensure(n int) error {
	if n < 1 {
		return fmt.Errorf("invalid capture body identifier")
	}
	i := (n - 1) % captureBodyShardCount
	item := &s.shards[i]
	if item.root != nil && item.dir != nil {
		return nil
	}
	name := captureShardName(i)
	if err := s.stage.Mkdir(name, 0700); err != nil {
		return err
	}
	var err error
	item.info, err = s.stage.Lstat(name)
	if err != nil {
		return err
	}
	item.root, err = captureOpenShard(s.stage, name)
	if err != nil {
		return err
	}
	item.dir, err = item.root.Open(".")
	if err != nil {
		return err
	}
	return s.validateShard(i)
}

func (s *archiveCaptureShards) validate() error {
	for i := range s.shards {
		item := &s.shards[i]
		if item.info == nil && item.root == nil && item.dir == nil {
			continue
		}
		if err := s.validateShard(i); err != nil {
			return err
		}
	}
	return nil
}

func (s *archiveCaptureShards) body(n int) (*os.Root, *os.File, string) {
	shard := &s.shards[(n-1)%captureBodyShardCount]
	return shard.root, shard.dir, captureBodyLeaf(n)
}

// Rooted handles keep cleanup inside the directories this invocation created.
// A replacement at a shard's former name is never followed or removed.
func (s *archiveCaptureShards) close() error {
	var result error
	for i := range s.shards {
		item := &s.shards[i]
		if item.dir != nil {
			result = errors.Join(result, item.dir.Close())
		}
		if item.root != nil {
			result = errors.Join(result, item.root.Close())
		}
		if item.info == nil {
			continue
		}
		name := captureShardName(i)
		live, err := s.stage.Lstat(name)
		if err == nil && live.IsDir() && live.Mode()&os.ModeSymlink == 0 && os.SameFile(item.info, live) {
			result = errors.Join(result, s.stage.Remove(name))
		} else {
			result = errors.Join(result, fmt.Errorf("capture body shard changed during cleanup"), err)
		}
	}
	return result
}

func (s *archiveCaptureShards) validateShard(i int) error {
	item := &s.shards[i]
	if item.info == nil || item.root == nil || item.dir == nil {
		return fmt.Errorf("capture body shard incomplete")
	}
	live, err := s.stage.Lstat(captureShardName(i))
	if err != nil {
		return err
	}
	opened, err := item.dir.Stat()
	if err != nil {
		return err
	}
	rooted, err := item.root.Stat(".")
	if err != nil {
		return err
	}
	for _, info := range []fs.FileInfo{live, opened, rooted} {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 || !os.SameFile(item.info, info) {
			return fmt.Errorf("capture body shard identity or permissions changed")
		}
	}
	return nil
}
