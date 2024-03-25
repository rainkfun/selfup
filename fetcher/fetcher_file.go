package fetcher

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/rainkfun/go-kit/hash"
)

// File checks the provided Path, at the provided
// Interval for new Go binaries. When a new binary
// is found it will replace the currently running
// binary.
type File struct {
	Path     string
	Interval time.Duration
	// hash is the file modify time and its size
	hash  string
	delay bool
}

// Init sets the Path and Interval options
func (f *File) Init() error {
	if f.Path == "" {
		return fmt.Errorf("Path required")
	}
	if f.Interval < 1*time.Second {
		f.Interval = 1 * time.Second
	}
	if err := f.updateHash(); err != nil {
		return err
	}
	return nil
}

// Fetch file from the specified Path
func (f *File) Fetch(binStat *BinStat) (io.Reader, error) {
	//only delay after first fetch
	if f.delay {
		time.Sleep(f.Interval)
	}
	f.delay = true
	if err := f.updateHash(); err != nil {
		return nil, err
	}
	// no change
	if binStat.Hash == f.hash {
		return nil, nil
	}
	// changed!
	file, err := os.Open(f.Path)
	if err != nil {
		return nil, err
	}
	//check every 1/4s for 5s to
	//ensure its not mid-copy
	const rate = 250 * time.Millisecond
	const total = int(5 * time.Second / rate)
	attempt := 1
	lastHash := f.hash
	for {
		if attempt == total {
			file.Close()
			return nil, errors.New("file is currently being changed")
		}
		attempt++
		//sleep
		time.Sleep(rate)
		//check hash!
		if err := f.updateHash(); err != nil {
			file.Close()
			return nil, err
		}
		//check until no longer changing
		if lastHash == f.hash {
			break
		}
		lastHash = f.hash
	}
	return file, nil
}

func (f *File) updateHash() error {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		//binary does not exist, skip
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("Read file error: %s", err)
	}

	digest, err := hash.XXH64SumString(data)
	if err != nil {
		return fmt.Errorf("Generate hash error: %s", err)
	}

	f.hash = digest
	return nil
}
