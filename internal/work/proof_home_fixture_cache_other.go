//go:build !darwin

package work

import "errors"

func defaultHomeFixtureCacheBase() (string, error) {
	return "", errors.New("home fixture accelerator requires Darwin")
}
