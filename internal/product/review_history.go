package product

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

// The existing review ledger remains the history owner. A short inode lock
// makes check+append atomic across different review operations and protects
// readers from observing an in-flight partial row. No candidate lock is held.
func withReviewHistory(homePath string, writing bool, fn func(*os.File, []Review) error) (resultErr error) {
	path := reviewsLog(homePath)
	flags, lock := os.O_RDONLY, syscall.LOCK_SH
	if writing {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		flags, lock = os.O_CREATE|os.O_RDWR|os.O_APPEND, syscall.LOCK_EX
	}
	f, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW, 0o644)
	if !writing && os.IsNotExist(err) {
		return fn(nil, nil)
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	if err := syscall.Flock(int(f.Fd()), lock); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, syscall.Flock(int(f.Fd()), syscall.LOCK_UN)) }()
	body, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	var reviews []Review
	seen := map[string]Review{}
	for i, line := range bytes.Split(body, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var review Review
		if err := json.Unmarshal(line, &review); err != nil || review.ID == "" {
			return fmt.Errorf("product review history row %d is incomplete or invalid: %v", i+1, err)
		}
		if previous, ok := seen[review.ID]; ok {
			if !sameReview(previous, review) {
				return fmt.Errorf("product review history has conflicting rows for %q", review.ID)
			}
			continue // retain historical bytes, expose one logical immutable row
		}
		seen[review.ID] = review
		reviews = append(reviews, review)
	}
	if err := fn(f, reviews); err != nil {
		return err
	}
	if writing {
		dir, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		return errors.Join(dir.Sync(), dir.Close())
	}
	return nil
}

func appendReviewRow(f *os.File, review Review) error {
	body, err := json.Marshal(review)
	if err != nil {
		return err
	}
	body = append(body, '\n')
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			body = append([]byte{'\n'}, body...)
		}
	}
	n, err := f.Write(body)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	return f.Sync()
}
