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
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"

	"github.com/Senetas/crypto-cli/crypto"
	"github.com/Senetas/crypto-cli/distribution"
	"github.com/Senetas/crypto-cli/registry/names"
	"github.com/Senetas/crypto-cli/utils"
)

// CreateManifest creates an unencrypted manifest (with the data necessary for encryption)
func CreateManifest(
	ref names.NamedTaggedRepository,
	opts *crypto.Opts,
) (
	manifest *distribution.ImageManifest,
	err error,
) {
	ctx := context.Background()

	// TODO: fix hardcoded version if necessary
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithVersion("1.37"))
	if err != nil {
		err = utils.StripTrace(errors.Wrap(err, "could not create client for docker daemon"))
		return
	}

	inspt, _, err := cli.ImageInspectWithRaw(ctx, ref.String())
	if err != nil {
		err = errors.WithStack(err)
		return
	}

	imageTar, err := cli.ImageSave(ctx, []string{inspt.ID})
	if err != nil {
		err = errors.WithStack(err)
		return
	}
	defer func() { err = utils.CheckedClose(imageTar, err) }()

	layers, err := layersToEncrypt(ctx, cli, inspt)
	if err != nil {
		return
	}

	log.Debug().Msgf("The following layers are to be encrypted: %v", layers)

	// output manifest
	manifest = &distribution.ImageManifest{
		SchemaVersion: 2,
		MediaType:     distribution.MediaTypeManifest,
		DirName:       filepath.Join(tempRoot, uuid.New().String()),
	}

	// extract image
	if err = extractTarBall(imageTar, manifest); err != nil {
		return
	}

	configBlob, layerBlobs, err := mkBlobs(ref.Path(), ref.Tag(), manifest.DirName, layers, opts)
	if err != nil {
		return
	}

	manifest.Config = configBlob
	manifest.Layers = layerBlobs

	return manifest, nil
}

func extractTarBall(r io.Reader, manifest *distribution.ImageManifest) (err error) {
	if err = os.MkdirAll(manifest.DirName, 0700); err != nil {
		return errors.Wrapf(err, "could not create: %s", manifest.DirName)
	}

	tarReader := tar.NewReader(r)

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		path := filepath.Join(manifest.DirName, header.Name)
		info := header.FileInfo()
		if info.IsDir() {
			if err = os.MkdirAll(path, info.Mode()); err != nil {
				return errors.WithStack(err)
			}
			continue
		}

		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		defer func() { err = file.Close() }()
		_, err = io.Copy(file, tarReader)
		if err != nil {
			return err
		}
	}
	return nil
}

// TODO: find a way to do this by interfacing with the daemon directly
func mkBlobs(
	repo, tag, path string,
	layers []string,
	opts *crypto.Opts,
) (
	configBlob distribution.Blob,
	layerBlobs []distribution.Blob,
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
		err = errors.Wrapf(err, "could not open file: %s", manifestfile)
		return
	}
	defer func() { err = utils.CheckedClose(manifestFH, err) }()

	image, err := mkArchiveStruct(path, manifestFH)
	if err != nil {
		return
	}

	switch opts.EncType {
	case crypto.Pbkdf2Aes256Gcm:
		return pbkdf2Aes256GcmEncrypt(path, layerSet, image, opts)
	case crypto.None:
		return noneEncrypt(path, layerSet, image, opts)
	default:
	}
	return nil, nil, errors.Errorf("%v is not a valid encryption type", opts.EncType)
}

func noneEncrypt(
	path string,
	layerSet map[string]bool,
	image *archiveStruct,
	opts *crypto.Opts,
) (
	distribution.Blob,
	[]distribution.Blob,
	error,
) {
	layerBlobs := make([]distribution.Blob, len(image.Layers))
	configBlob := distribution.NewPlainConfig(filepath.Join(path, image.Config), "", 0)
	for i, f := range image.Layers {
		layerBlobs[i] = distribution.NewPlainLayer(filepath.Join(path, f), "", 0)
	}
	return configBlob, layerBlobs, nil
}

func pbkdf2Aes256GcmEncrypt(
	path string,
	layerSet map[string]bool,
	image *archiveStruct,
	opts *crypto.Opts,
) (
	_ distribution.Blob,
	_ []distribution.Blob,
	err error,
) {
	// make the config
	dec, err := distribution.NewDecrypto(opts)
	if err != nil {
		return nil, nil, err
	}
	configBlob := distribution.NewConfig(filepath.Join(path, image.Config), "", 0, dec)

	layerBlobs := make([]distribution.Blob, len(image.Layers))
	for i, f := range image.Layers {
		basename := filepath.Join(path, f)

		dec, err := distribution.NewDecrypto(opts)
		if err != nil {
			return nil, nil, err
		}

		d, err := fileDigest(basename)
		if err != nil {
			return nil, nil, errors.WithStack(err)
		}

		log.Debug().Msgf("preparing %s", d)
		if layerSet[d.String()] {
			layerBlobs[i] = distribution.NewLayer(filepath.Join(path, f), d, 0, dec)
		} else {
			layerBlobs[i] = distribution.NewPlainLayer(filepath.Join(path, f), d, 0)
		}
	}

	return configBlob, layerBlobs, nil
}

func fileDigest(filename string) (d digest.Digest, err error) {
	fh, err := os.Open(filename)
	if err != nil {
		return
	}
	defer func() { err = utils.CheckedClose(fh, err) }()

	return digest.Canonical.FromReader(fh)
}

func layersToEncrypt(ctx context.Context, cli *client.Client, inspt types.ImageInspect) ([]string, error) {
	// get the history
	hist, err := cli.ImageHistory(ctx, inspt.ID)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// the positions of the layers to encrypt
	eps, err := encryptPositions(hist)
	if err != nil {
		return nil, err
	}

	log.Debug().Msgf("%v", eps)
	log.Debug().Msgf("%v", inspt.RootFS.Layers)

	// the total number of layers
	diffIDsToEncrypt := make([]string, len(eps))
	for i, n := range eps {
		diffIDsToEncrypt[i] = inspt.RootFS.Layers[n]
	}

	log.Debug().Msgf("%v", diffIDsToEncrypt)

	// the last n entries in this array are the diffIDs of the layers to encrypt
	return diffIDsToEncrypt, nil
}

func encryptPositions(hist []image.HistoryResponseItem) (encryptPos []int, err error) {
	n := 0
	toEncrypt := false
	createdRE := `#\(nop\)\s+` + labelString + `=(true|false)|(#\(nop\))`
	re := regexp.MustCompile(createdRE)

	for i := len(hist) - 1; i >= 0; i-- {
		matches := re.FindSubmatch([]byte(hist[i].CreatedBy))

		if hist[i].Size != 0 || len(matches) == 0 {
			if toEncrypt {
				encryptPos = append(encryptPos, n)
			}
			n++
		} else {
			switch string(matches[1]) {
			case "true":
				toEncrypt = true
			case "false":
				toEncrypt = false
			default:
			}
		}
	}

	if len(encryptPos) == 0 {
		return nil, utils.NewError("this image was not built with the correct LABEL", false)
	}

	return encryptPos, nil
}

type archiveStruct struct {
	Config string
	Layers []string
}

func mkArchiveStruct(path string, manifestFH io.Reader) (*archiveStruct, error) {
	var images []*archiveStruct
	dec := json.NewDecoder(manifestFH)
	if err := dec.Decode(&images); err != nil {
		return nil, errors.Wrapf(err, "error unmarshalling manifest")
	}

	if len(images) < 1 {
		return nil, errors.New("no image data was found")
	}

	return images[0], nil
}
