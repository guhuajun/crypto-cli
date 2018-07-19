// Copyright © 2018 SENETAS SECURITY PTY LTD
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

package images

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"
	tarinator "github.com/verybluebot/tarinator-go"

	"github.com/Senetas/crypto-cli/crypto"
	"github.com/Senetas/crypto-cli/distribution"
	"github.com/Senetas/crypto-cli/registry/names"
	"github.com/Senetas/crypto-cli/utils"
)

// CreateManifest creates a manifest and encrypts all necessary parts of it
// These are then ready to be uploaded to a regitry
func CreateManifest(
	ref names.NamedTaggedRepository,
	opts crypto.Opts,
) (
	manifest *distribution.ImageManifest,
	err error,
) {
	ctx := context.Background()

	// TODO: fix hardcoded version/ check if necessary
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.37"))
	if err != nil {
		return nil, utils.StripTrace(errors.Wrap(err, "could not create client for docker daemon"))
	}

	inspt, _, err := cli.ImageInspectWithRaw(ctx, ref.String())
	if err != nil {
		return nil, errors.WithStack(err)
	}

	tarFH, err := cli.ImageSave(ctx, []string{inspt.ID})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	layers, err := layersToEncrypt(ctx, cli, inspt)
	if err != nil {
		return nil, err
	}
	defer func() { err = utils.CheckedClose(tarFH, err) }()

	log.Info().Msgf("The following layers are to be encrypted: %v", layers)

	// output image
	manifest = &distribution.ImageManifest{
		SchemaVersion: 2,
		MediaType:     distribution.MediaTypeManifest,
		DirName:       filepath.Join(tempRoot, uuid.New().String()),
	}

	// extract image
	if err = extractTarBall(tarFH, manifest); err != nil {
		return nil, err
	}

	configData, layerData, err := findLayers(ref.Path(), ref.Tag(), manifest.DirName, layers)
	if err != nil {
		return nil, err
	}

	manifest.Config = configData
	manifest.Layers = layerData

	if err = encryptKeys(ref, manifest, opts); err != nil {
		return nil, err
	}

	return manifest, nil
}

func encryptKeys(
	ref names.NamedTaggedRepository,
	manifest *distribution.ImageManifest,
	opts crypto.Opts,
) error {
	opts.Salt = fmt.Sprintf(configSalt, ref.Path(), ref.Tag())
	if err := manifest.Config.Encrypt(opts); err != nil {
		return err
	}

	for i, l := range manifest.Layers {
		opts.Salt = fmt.Sprintf(layerSalt, ref.Path(), ref.Tag(), i)
		if l.Crypto != nil {
			if err := l.Encrypt(opts); err != nil {
				return err
			}
		}
	}

	return nil
}

func layersToEncrypt(ctx context.Context, cli *client.Client, inspt types.ImageInspect) ([]string, error) {
	// get the history
	hist, err := cli.ImageHistory(ctx, inspt.ID)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// the number of layers to encrypt
	n, err := numLayers(hist)
	if err != nil {
		return nil, err
	}

	// the total number of layers
	m := len(inspt.RootFS.Layers)

	// the last n entries in this array are the diffIDs of the layers to encrypt
	return inspt.RootFS.Layers[m-n:], nil
}

func extractTarBall(tarFH io.Reader, manifest *distribution.ImageManifest) (err error) {
	tarfile := manifest.DirName + ".tar"

	if err = os.MkdirAll(manifest.DirName, 0700); err != nil {
		return errors.Wrapf(err, "could not create: %s", manifest.DirName)
	}

	outFH, err := os.Create(tarfile)
	if err != nil {
		return errors.Wrapf(err, "could not create: %s", tarfile)
	}
	defer func() { err = utils.CheckedClose(outFH, err) }()

	if _, err = io.Copy(outFH, tarFH); err != nil {
		return errors.Wrapf(err, "could not extract to %s", tarfile)
	}

	if err = outFH.Sync(); err != nil {
		return errors.Wrapf(err, "could not sync file: %s", tarfile)
	}

	if err = tarinator.UnTarinate(manifest.DirName, tarfile); err != nil {
		return err
	}

	return nil
}

type configLayers struct {
	Config string
	Layers []string
}

// find the layer files that correponds to the digests we want to encrypt
// TODO: find a way to do this by interfacing with the daemon directly
func findLayers(
	repo, tag, path string,
	layers []string,
) (
	config *distribution.Layer,
	layer []*distribution.Layer,
	err error,
) {
	// assemble layers
	layerSet := make(map[string]bool)
	for _, x := range layers {
		layerSet[x] = true
	}

	// read the archive manifest
	manifestfile := filepath.Join(path, "manifest.json")
	manifestFH, err := os.Open(manifestfile)
	if err != nil {
		return nil, nil, errors.Wrapf(err, "could not open file: %s", manifestfile)
	}
	defer func() { err = utils.CheckedClose(manifestFH, err) }()

	images, configfile, err := mkConfig(path, manifestFH)
	if err != nil {
		return nil, nil, err
	}

	filename, d, size, key, err := encryptLayer(configfile)
	if err != nil {
		return nil, nil, err
	}

	config = distribution.NewConfig(filename, d, size, key)

	layer = make([]*distribution.Layer, len(images[0].Layers))
	for i, f := range images[0].Layers {
		basename := filepath.Join(path, f)

		d, err = fileDigest(basename)
		if err != nil {
			return nil, nil, err
		}

		if layerSet[d.String()] {
			log.Info().Msgf("encrypting %s", d)
			filename, d, size, key, err := encryptLayer(basename)
			if err != nil {
				return nil, nil, err
			}
			layer[i] = distribution.NewLayer(filename, d, size, key)
		} else {
			filename, d, size, err := compressLayer(basename)
			if err != nil {
				return nil, nil, err
			}
			layer[i] = distribution.NewPlainLayer(filename, d, size)
		}
	}

	return config, layer, nil
}

func fileDigest(filename string) (*digest.Digest, error) {
	fh, err := os.Open(filename)
	if err != nil {
		return nil, errors.Wrapf(err, "could not open file: %s", filename)
	}

	d, err := digest.Canonical.FromReader(fh)
	if err != nil {
		return nil, errors.Wrapf(err, "could not calculate digest: %s", filename)
	}

	return &d, nil
}

func numLayers(hist []image.HistoryResponseItem) (n int, err error) {
	for _, h := range hist {
		if h.Size != 0 || !strings.Contains(h.CreatedBy, "#(nop)") {
			n++
		} else if strings.Contains(h.CreatedBy, labelString) {
			return n, nil
		}
	}
	return 0, utils.NewError("this image was not built with the correct LABEL", false)
}

func compressLayer(filename string) (compFile string, d *digest.Digest, size int64, err error) {
	compFile = filename + ".gz"

	d, err = utils.CompressWithDigest(filename)
	if err != nil {
		return "", nil, 0, err
	}

	stat, err := os.Stat(compFile)
	if err != nil {
		return "", nil, 0, errors.Wrapf(err, "could not stat file: %s", compFile)
	}

	return compFile, d, stat.Size(), nil
}

func encryptLayer(filename string) (encname string, d *digest.Digest, size int64, key []byte, err error) {
	compname := filename + ".gz"
	encname = compname + ".aes"

	if err = utils.Compress(filename); err != nil {
		return "", nil, 0, nil, err
	}

	key, err = crypto.GenDataKey()
	if err != nil {
		return "", nil, 0, nil, err
	}

	d, size, err = crypto.EncFile(compname, encname, key)
	if err != nil {
		return "", nil, 0, nil, err
	}

	return encname, d, size, key, nil
}

func mkConfig(path string, manifestFH io.Reader) ([]*configLayers, string, error) {
	var images []*configLayers
	dec := json.NewDecoder(manifestFH)
	if err := dec.Decode(&images); err != nil {
		return nil, "", errors.Wrapf(err, "error unmarshalling manifest")
	}

	if len(images) < 1 {
		return nil, "", errors.New("no image data was found")
	}

	configfile := filepath.Join(path, images[0].Config)
	return images, configfile, nil
}
