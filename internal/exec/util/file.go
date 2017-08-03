// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"encoding/hex"
	"hash"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/coreos/ignition/config/types"
	"github.com/coreos/ignition/internal/log"
	"github.com/coreos/ignition/internal/resource"
	internalUtil "github.com/coreos/ignition/internal/util"
)

const (
	DefaultDirectoryPermissions os.FileMode = 0755
	DefaultFilePermissions      os.FileMode = 0644
)

type FetchOp struct {
	Hash         hash.Hash
	Path         string
	Mode         os.FileMode
	Uid          int
	Gid          int
	Url          url.URL
	FetchOptions resource.FetchOptions
}

// newHashedReader returns a new ReadCloser that also writes to the provided hash.
func newHashedReader(reader io.ReadCloser, hasher hash.Hash) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{
		Reader: io.TeeReader(reader, hasher),
		Closer: reader,
	}
}

// PrepareFetch converts a given logger, http client, and types.File into a
// FetchOp. This includes operations such as parsing the source URL, generating
// a hasher, and performing user/group name lookups. If an error is encountered,
// the issue will be logged and nil will be returned.
func (u Util) PrepareFetch(l *log.Logger, f types.File) *FetchOp {
	var err error
	var expectedSum []byte

	// explicitly ignoring the error here because the config should already be
	// validated by this point
	uri, _ := url.Parse(f.Contents.Source)

	hasher, err := GetHasher(f.Contents.Verification)
	if err != nil {
		l.Crit("Error verifying file %q: %v", f.Path, err)
		return nil
	}

	if hasher != nil {
		// explicitly ignoring the error here because the config should already
		// be validated by this point
		_, expectedSumString, _ := f.Contents.Verification.HashParts()
		expectedSum, err = hex.DecodeString(expectedSumString)
		if err != nil {
			l.Crit("Error parsing verification string %q: %v", expectedSumString, err)
			return nil
		}
	}

	f.User.ID, f.Group.ID = u.GetUserGroupID(l, f.User, f.Group)

	return &FetchOp{
		Path: f.Path,
		Hash: hasher,
		Mode: os.FileMode(f.Mode),
		Uid:  *f.User.ID,
		Gid:  *f.Group.ID,
		Url:  *uri,
		FetchOptions: resource.FetchOptions{
			Hash:        hasher,
			Compression: f.Contents.Compression,
			ExpectedSum: expectedSum,
		},
	}
}

func (u Util) WriteLink(s types.Link) error {
	path := u.JoinPath(s.Path)

	if err := MkdirForFile(path); err != nil {
		return err
	}

	if s.Hard {
		targetPath := u.JoinPath(s.Target)
		return os.Link(targetPath, path)
	}

	if err := os.Symlink(s.Target, path); err != nil {
		return err
	}

	if err := os.Chown(path, *s.User.ID, *s.Group.ID); err != nil {
		return err
	}

	return nil
}

// PerformFetch performs a fetch operation generated by PrepareFetch, retrieving
// the file and writing it to disk. Any encountered errors are returned.
func (u Util) PerformFetch(f *FetchOp) error {
	var err error

	path := u.JoinPath(string(f.Path))

	if err := MkdirForFile(path); err != nil {
		return err
	}

	// Create a temporary file in the same directory to ensure it's on the same filesystem
	var tmp *os.File
	if tmp, err = ioutil.TempFile(filepath.Dir(path), "tmp"); err != nil {
		return err
	}

	defer func() {
		tmp.Close()
		if err != nil {
			os.Remove(tmp.Name())
		}
	}()

	err = u.Fetcher.Fetch(f.Url, tmp, f.FetchOptions)
	if err != nil {
		u.Crit("Error fetching file %q: %v", f.Path, err)
		return err
	}

	// XXX(vc): Note that we assume to be operating on the file we just wrote, this is only guaranteed
	// by using syscall.Fchown() and syscall.Fchmod()

	// Ensure the ownership and mode are as requested (since WriteFile can be affected by sticky bit)
	if err = os.Chown(tmp.Name(), f.Uid, f.Gid); err != nil {
		return err
	}

	if err = os.Chmod(tmp.Name(), f.Mode); err != nil {
		return err
	}

	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}

	return nil
}

func (u Util) GetUserGroupID(l *log.Logger, user types.NodeUser, group types.NodeGroup) (*int, *int) {
	if user.Name != "" {
		usr, err := u.userLookup(user.Name)
		if err != nil {
			l.Crit("No such user %q: %v", user.Name, err)
			return nil, nil
		}
		uid, err := strconv.ParseInt(usr.Uid, 0, 0)
		if err != nil {
			l.Crit("Couldn't parse uid %q: %v", usr.Uid, err)
			return nil, nil
		}
		tmp := int(uid)
		user.ID = &tmp
	}
	if group.Name != "" {
		g, err := u.groupLookup(group.Name)
		if err != nil {
			l.Crit("No such group %q: %v", group.Name, err)
			return nil, nil
		}
		gid, err := strconv.ParseInt(g.Gid, 0, 0)
		if err != nil {
			l.Crit("Couldn't parse gid %q: %v", g.Gid, err)
			return nil, nil
		}
		tmp := int(gid)
		group.ID = &tmp
	}

	if user.ID == nil {
		user.ID = internalUtil.IntToPtr(0)
	}
	if group.ID == nil {
		group.ID = internalUtil.IntToPtr(0)
	}

	return user.ID, group.ID
}

// MkdirForFile helper creates the directory components of path.
func MkdirForFile(path string) error {
	return os.MkdirAll(filepath.Dir(path), DefaultDirectoryPermissions)
}
