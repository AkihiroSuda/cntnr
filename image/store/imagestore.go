package store

import (
	"os"
	"path/filepath"
	"time"

	"github.com/containers/image/types"
	"github.com/mgoltzsche/cntnr/image"
	exterrors "github.com/mgoltzsche/cntnr/pkg/errors"
	"github.com/mgoltzsche/cntnr/pkg/lock"
	"github.com/mgoltzsche/cntnr/pkg/log"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

const (
	AnnotationImported = "com.github.mgoltzsche.cntnr.image.imported"
)

var _ image.ImageStore = &ImageStore{}

type ImageStore struct {
	lock lock.ExclusiveLocker
	*ImageStoreRO
	fsCache       *ImageFSROCache
	temp          string
	systemContext *types.SystemContext
	trustPolicy   TrustPolicyContext
	rootless      bool
	loggers       log.Loggers
}

func NewImageStore(store *ImageStoreRO, fsCache *ImageFSROCache, temp string, systemContext *types.SystemContext, trustPolicy TrustPolicyContext, rootless bool, loggers log.Loggers) (*ImageStore, error) {
	lck, err := lock.NewExclusiveDirLocker(filepath.Join(os.TempDir(), "cntnr", "lock"))
	if err != nil {
		err = errors.Wrap(err, "new image store")
	}
	return &ImageStore{lck, store, fsCache, temp, systemContext, trustPolicy, rootless, loggers}, err
}

func (s *ImageStore) OpenLockedImageStore() (image.ImageStoreRW, error) {
	return s.openLockedImageStore(s.lock.NewSharedLocker())
}

func (s *ImageStore) openLockedImageStore(locker lock.Locker) (image.ImageStoreRW, error) {
	return NewImageStoreRW(locker, s.ImageStoreRO, s.fsCache, s.temp, s.systemContext, s.trustPolicy, s.rootless, s.loggers)
}

func (s *ImageStore) DelImage(ids ...digest.Digest) (err error) {
	defer exterrors.Wrapd(&err, "del image")
	lockedStore, err := s.openLockedImageStore(s.lock)
	if err != nil {
		return
	}
	defer func() {
		err = exterrors.Append(err, lockedStore.Close())
	}()

	imgs, err := lockedStore.Images()
	if err != nil {
		return
	}
	for _, id := range ids {
		for _, img := range imgs {
			if id == img.ID() && img.Repo != "" {
				// TODO: Use TagName struct as argument
				// TODO: single delete batch per repository
				if err = lockedStore.UntagImage(img.Repo + ":" + img.Ref); err != nil {
					return
				}
			}
		}
		if err = s.imageIds.Del(id); err != nil {
			return
		}
	}
	return
}

func (s *ImageStore) ImageGC(before time.Time) (err error) {
	defer exterrors.Wrapd(&err, "image gc")
	lockedStore, err := s.openLockedImageStore(s.lock)
	if err != nil {
		return
	}
	defer func() {
		err = exterrors.Append(err, lockedStore.Close())
	}()

	// Collect all image IDs and delete
	keep := map[digest.Digest]bool{}
	delIDs := map[digest.Digest]bool{}
	imgs, err := s.Images()
	if err != nil {
		return
	}
	for _, img := range imgs {
		if img.LastUsed.Before(before) {
			if img.Repo != "" {
				// TODO: single delete batch per repository
				if err = lockedStore.UntagImage(img.Repo + ":" + img.Ref); err != nil {
					return
				}
			}
			delIDs[img.ID()] = true
		} else {
			keep[img.ManifestDigest] = true
			keep[img.Manifest.Config.Digest] = true
			for _, l := range img.Manifest.Layers {
				keep[l.Digest] = true
			}
		}
	}

	// Delete image IDs
	for delID, _ := range delIDs {
		if err = s.imageIds.Del(delID); err != nil {
			return
		}
	}

	// Delete all but the named blobs
	return s.blobs.RetainBlobs(keep)
}
