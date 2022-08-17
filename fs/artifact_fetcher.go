/*
   Copyright The Soci Snapshotter Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package fs

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"

	"github.com/awslabs/soci-snapshotter/service/keychain/dockerconfig"
	"github.com/awslabs/soci-snapshotter/soci"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/reference"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	ctrdockerconfig "github.com/containerd/containerd/remotes/docker/config"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

type Fetcher interface {
	// Fetch fetches the artifact identified by the descriptor. It first checks the local content store
	// and returns a `ReadCloser` from there. Otherwise it fetches from the remote, saves in the local content store
	// and then returns a `ReadCloser`.
	Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, bool, error)
	// Store takes in a descriptor and io.Reader and stores it in the local store.
	Store(ctx context.Context, desc ocispec.Descriptor, reader io.Reader) error
}

// artifactFetcher is responsible for fetching and storing artifacts in the provided artifact store.
type artifactFetcher struct {
	resolver    remotes.Resolver
	remoteStore content.Storage
	localStore  content.Storage
	refspec     reference.Spec
}

// Constructs a new artifact fetcher
// Takes in the image reference, the local store and the resolver
func newArtifactFetcher(refspec reference.Spec, localStore, remoteStore content.Storage, resolver remotes.Resolver) (*artifactFetcher, error) {
	return &artifactFetcher{
		resolver:    resolver,
		localStore:  localStore,
		remoteStore: remoteStore,
		refspec:     refspec,
	}, nil
}

func newRemoteStore(refspec reference.Spec) (content.Storage, error) {
	repo, err := remote.NewRepository(refspec.Locator)
	if err != nil {
		return nil, fmt.Errorf("cannot create repository %s: %w", refspec.Locator, err)
	}

	authClient := auth.DefaultClient
	authClient.Cache = auth.DefaultCache
	authClient.Credential = func(_ context.Context, host string) (auth.Credential, error) {
		username, secret, err := dockerconfig.DockerCreds(host)
		if err != nil {
			return auth.EmptyCredential, err
		}
		if username == "" && secret != "" {
			return auth.Credential{
				RefreshToken: secret,
			}, nil
		}

		return auth.Credential{
			Username: username,
			Password: secret,
		}, nil
	}

	repo.Client = authClient
	return repo, nil
}

// Constructs a new resolver for Docker registries
func newResolver() remotes.Resolver {
	options := docker.ResolverOptions{
		Tracker: docker.NewInMemoryTracker(),
	}
	hostOptions := ctrdockerconfig.HostOptions{}
	hostOptions.Credentials = dockerconfig.DockerCreds
	hostOptions.DefaultTLS = &tls.Config{}
	options.Hosts = ctrdockerconfig.ConfigureHosts(context.Background(), hostOptions)
	return docker.NewResolver(options)
}

// Takes in a descriptor and returns the associated ref to fetch from remote.
// i.e. <hostname>/<repo>@<digest>
func (f *artifactFetcher) constructRef(desc ocispec.Descriptor) string {
	return fmt.Sprintf("%s@%s", f.refspec.Locator, desc.Digest.String())
}

// Fetches the artifact identified by the descriptor.
// It first checks the local store for the artifact.
// If not found, if constructs the ref and fetches it from remote.
func (f *artifactFetcher) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, bool, error) {

	// Check local store first
	rc, err := f.localStore.Fetch(ctx, desc)
	if err == nil {
		return rc, true, nil
	}

	log.G(ctx).WithField("digest", desc.Digest.String()).Infof("fetching artifact from remote")
	if desc.Size == 0 {
		// Digest verification fails is desc.Size == 0
		// Therefore, we try to use the resolver to resolve the descriptor
		// and hopefully get the size.
		// Note that the resolve would fail for size > 4MiB, since that's the limit
		// for the manifest size when using the Docker resolver.
		log.G(ctx).WithField("digest", desc.Digest).Warnf("size of descriptor is 0, trying to resolve it...")
		desc, err = f.resolve(ctx, desc)
		if err != nil {
			return nil, false, fmt.Errorf("size of descriptor is 0; unable to resolve: %w", err)
		}
	}

	rc, err = f.remoteStore.Fetch(ctx, desc)
	if err != nil {
		return nil, false, fmt.Errorf("unable to fetch descriptor (%v) from remote store: %w", desc.Digest, err)
	}

	return rc, false, nil
}

func (f *artifactFetcher) resolve(ctx context.Context, desc ocispec.Descriptor) (ocispec.Descriptor, error) {
	ref := f.constructRef(desc)
	_, desc, err := f.resolver.Resolve(ctx, ref)
	if err != nil {
		return desc, fmt.Errorf("unable to resolve ref (%s): %w", ref, err)
	}
	return desc, nil
}

// Store takes in an descriptor and io.Reader and stores it in the local store.
func (f *artifactFetcher) Store(ctx context.Context, desc ocispec.Descriptor, reader io.Reader) error {
	err := f.localStore.Push(ctx, desc, reader)
	if err != nil {
		return fmt.Errorf("unable to push to local store: %w", err)
	}
	return nil
}

func FetchSociArtifacts(ctx context.Context, imageRef, indexDigest string, store content.Storage) (*soci.SociIndex, error) {
	refspec, err := reference.Parse(imageRef)
	if err != nil {
		return nil, fmt.Errorf("cannot parse image ref (%s): %w", imageRef, err)
	}
	remoteStore, err := newRemoteStore(refspec)
	if err != nil {
		return nil, fmt.Errorf("cannot create remote store: %w", err)
	}
	fetcher, err := newArtifactFetcher(refspec, store, remoteStore, newResolver())
	if err != nil {
		return nil, fmt.Errorf("could not create an artifact fetcher: %w", err)
	}

	log.G(ctx).WithField("digest", indexDigest).Infof("fetching SOCI index from remote registry")
	dgst, err := digest.Parse(indexDigest)
	if err != nil {
		log.G(ctx).WithField("digest", indexDigest).Warnf("could not parse soci index digest")
		return nil, err
	}

	indexReader, local, err := fetcher.Fetch(ctx, ocispec.Descriptor{Digest: dgst})
	if err != nil {
		return nil, fmt.Errorf("unable to fetch SOCI index: %w", err)
	}

	var index soci.SociIndex
	buffer := new(bytes.Buffer)
	_, err = io.Copy(buffer, indexReader)
	indexReader.Close()
	if err != nil {
		return nil, err
	}
	indexBytes := buffer.Bytes()
	if err := json.Unmarshal(indexBytes, &index); err != nil {
		return nil, err
	}

	if !local {
		err = store.Push(ctx, ocispec.Descriptor{
			Digest: dgst,
			Size:   int64(len(buffer.Bytes())),
		}, buffer)

		if err != nil {
			log.G(ctx).Warnf("unable to store SOCI index into local store")
			return nil, err
		}
	}

	eg, ctx := errgroup.WithContext(ctx)
	for _, blob := range index.Blobs {
		blob := blob
		eg.Go(func() error {
			rc, local, err := fetcher.Fetch(ctx, blob)
			if err != nil {
				return fmt.Errorf("cannot fetch artifact: %w", err)
			}
			defer rc.Close()
			if local {
				return nil
			}
			return fetcher.Store(ctx, blob, rc)
		})
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return &index, nil
}
