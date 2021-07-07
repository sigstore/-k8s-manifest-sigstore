//
// Copyright 2020 IBM Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package k8smanifest

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"

	k8smnfcosign "github.com/sigstore/k8s-manifest-sigstore/pkg/cosign"
	k8smnfutil "github.com/sigstore/k8s-manifest-sigstore/pkg/util"
	mapnode "github.com/sigstore/k8s-manifest-sigstore/pkg/util/mapnode"
)

var EmbeddedAnnotationMaskKeys = []string{
	fmt.Sprintf("metadata.annotations.\"%s\"", ImageRefAnnotationKey),
	fmt.Sprintf("metadata.annotations.\"%s\"", SignatureAnnotationKey),
	fmt.Sprintf("metadata.annotations.\"%s\"", CertificateAnnotationKey),
	fmt.Sprintf("metadata.annotations.\"%s\"", MessageAnnotationKey),
	fmt.Sprintf("metadata.annotations.\"%s\"", BundleAnnotationKey),
}

var onMemoryCacheForVerifyResource *k8smnfutil.OnMemoryCache

func init() {
	onMemoryCacheForVerifyResource = &k8smnfutil.OnMemoryCache{TTL: 30 * time.Second}
}

type SignatureVerifier interface {
	Verify() (bool, string, *int64, error)
}

func NewSignatureVerifier(objYAMLBytes []byte, imageRef string, pubkeyPath *string) SignatureVerifier {
	i := &ImageSignatureVerifier{onMemoryCacheEnabled: true}
	var annotations map[string]string
	if imageRef == "" {
		annotations = k8smnfutil.GetAnnotationsInYAML(objYAMLBytes)
		if annoImageRef, ok := annotations[ImageRefAnnotationKey]; ok {
			imageRef = annoImageRef
		}
	}

	i.imageRef = imageRef

	if pubkeyPath != nil && *pubkeyPath != "" {
		i.pubkeyPath = pubkeyPath
	}

	if imageRef == "" {
		return &AnnotationSignatureVerifier{}
	} else {
		return i
	}
}

type ImageSignatureVerifier struct {
	imageRef             string
	pubkeyPath           *string
	onMemoryCacheEnabled bool
}

func (v *ImageSignatureVerifier) Verify() (bool, string, *int64, error) {
	imageRef := v.imageRef
	if imageRef == "" {
		return false, "", nil, errors.New("no image reference is found")
	}

	if v.onMemoryCacheEnabled {
		// try getting result from on-memory cache
		cacheFound, verified, signerName, signedTimestamp, err := v.getResultFromMemCache(imageRef)
		// if found, return it
		if cacheFound {
			return verified, signerName, signedTimestamp, err
		}
		// otherwise, do normal image verification
		verified, signerName, signedTimestamp, err = k8smnfcosign.VerifyImage(imageRef, v.pubkeyPath)
		// set the result to on-memory cache
		v.setResultToMemCache(imageRef, verified, signerName, signedTimestamp, err)
		return verified, signerName, signedTimestamp, err
	} else {
		// do normal image verification
		return k8smnfcosign.VerifyImage(imageRef, v.pubkeyPath)
	}
}

func (v *ImageSignatureVerifier) getResultFromMemCache(imageRef string) (bool, bool, string, *int64, error) {
	key := fmt.Sprintf("cache/verify-image/%s", imageRef)
	result, err := onMemoryCacheForVerifyResource.Get(key)
	if err != nil {
		// OnMemoryCache.Get() returns an error only when the key was not found
		return false, false, "", nil, nil
	}
	if len(result) != 3 {
		return false, false, "", nil, fmt.Errorf("cache returns inconsistent data: a length of verify image result must be 3, but got %v", len(result))
	}
	verified := false
	signerName := ""
	var signedTimestamp *int64
	if result[0] != nil {
		verified = result[0].(bool)
	}
	if result[1] != nil {
		signerName = result[1].(string)
	}
	if result[2] != nil {
		signedTimestamp = result[2].(*int64)
	}
	if result[3] != nil {
		err = result[3].(error)
	}
	return true, verified, signerName, signedTimestamp, err
}

func (v *ImageSignatureVerifier) setResultToMemCache(imageRef string, verified bool, signerName string, signedTimestamp *int64, err error) {
	key := fmt.Sprintf("cache/verify-image/%s", imageRef)
	_ = onMemoryCacheForVerifyResource.Set(key, verified, signerName, signedTimestamp, err)
}

type AnnotationSignatureVerifier struct {
}

func (v *AnnotationSignatureVerifier) Verify() (bool, string, *int64, error) {
	// TODO: support annotation signature
	return false, "", nil, errors.New("annotation-embedded signature is not supported yet")
}

// This is an interface for fetching YAML manifest
// a function Fetch() fetches a YAML manifest which matches the input object's kind, name and so on
type ManifestFetcher interface {
	Fetch(objYAMLBytes []byte) ([]byte, string, error)
}

func NewManifestFetcher(imageRef string) ManifestFetcher {
	if imageRef == "" {
		return &AnnotationManifestFetcher{}
	} else {
		return &ImageManifestFetcher{imageRefString: imageRef, onMemoryCacheEnabled: true}
	}
}

// ImageManifestFetcher is a fetcher implementation for image reference
type ImageManifestFetcher struct {
	imageRefString       string
	onMemoryCacheEnabled bool
}

func (f *ImageManifestFetcher) Fetch(objYAMLBytes []byte) ([]byte, string, error) {
	imageRefString := f.imageRefString
	if imageRefString == "" {
		annotations := k8smnfutil.GetAnnotationsInYAML(objYAMLBytes)
		if annoImageRef, ok := annotations[ImageRefAnnotationKey]; ok {
			imageRefString = annoImageRef
		}
	}
	if imageRefString == "" {
		return nil, "", errors.New("no image reference is found")
	}

	imageRefList := splitImageRefString(imageRefString)
	for _, imageRef := range imageRefList {
		concatYAMLbytes, err := f.fetchManifestInSingleImage(imageRef)
		if err != nil {
			return nil, "", err
		}
		found, foundManifest := k8smnfutil.FindManifestYAML(concatYAMLbytes, objYAMLBytes)
		if found {
			return foundManifest, imageRef, nil
		}
	}
	return nil, "", errors.New("failed to find a YAML manifest in the image")
}

func (f *ImageManifestFetcher) fetchManifestInSingleImage(singleImageRef string) ([]byte, error) {
	var concatYAMLbytes []byte
	var err error
	if f.onMemoryCacheEnabled {
		cacheFound := false
		// try getting YAML manifests from on-memory cache
		cacheFound, concatYAMLbytes, err = f.getManifestFromMemCache(singleImageRef)
		// if cache not found, do fetch and set the result to cache
		if !cacheFound {
			// fetch YAML manifests from actual image
			concatYAMLbytes, err = f.getConcatYAMLFromImageRef(singleImageRef)
			if err == nil {
				// set the result to on-memory cache
				f.setManifestToMemCache(singleImageRef, concatYAMLbytes, err)
			}
		}
	} else {
		// fetch YAML manifests from actual image
		concatYAMLbytes, err = f.getConcatYAMLFromImageRef(singleImageRef)
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to get YAMLs in the image")
	}
	return concatYAMLbytes, nil
}

func (f *ImageManifestFetcher) FetchAll() ([][]byte, error) {
	imageRefString := f.imageRefString
	imageRefList := splitImageRefString(imageRefString)

	yamls := [][]byte{}
	for _, imageRef := range imageRefList {
		concatYAMLbytes, err := f.fetchManifestInSingleImage(imageRef)
		if err != nil {
			return nil, err
		}
		yamlsInImage := k8smnfutil.SplitConcatYAMLs(concatYAMLbytes)
		yamls = append(yamls, yamlsInImage...)
	}
	return yamls, nil
}

func splitImageRefString(imageRefString string) []string {
	imageRefList := strings.Split(imageRefString, ",")
	for i := range imageRefList {
		imageRefList[i] = strings.TrimSpace(imageRefList[i])
	}
	return imageRefList
}

func (f *ImageManifestFetcher) getConcatYAMLFromImageRef(imageRef string) ([]byte, error) {
	image, err := k8smnfutil.PullImage(imageRef)
	if err != nil {
		return nil, err
	}
	concatYAMLbytes, err := k8smnfutil.GenerateConcatYAMLsFromImage(image)
	if err != nil {
		return nil, err
	}
	return concatYAMLbytes, nil
}

func (f *ImageManifestFetcher) getManifestFromMemCache(imageRef string) (bool, []byte, error) {
	key := fmt.Sprintf("cache/fetch-manifest/%s", imageRef)
	result, err := onMemoryCacheForVerifyResource.Get(key)
	if err != nil {
		// OnMemoryCache.Get() returns an error only when the key was not found
		return false, nil, nil
	}
	if len(result) != 2 {
		return false, nil, fmt.Errorf("cache returns inconsistent data: a length of fetch manifest result must be 2, but got %v", len(result))
	}
	var concatYAMLbytes []byte
	if result[0] != nil {
		concatYAMLbytes = result[0].([]byte)
	}
	if result[1] != nil {
		err = result[1].(error)
	}
	return true, concatYAMLbytes, err
}

func (f *ImageManifestFetcher) setManifestToMemCache(imageRef string, concatYAMLbytes []byte, err error) {
	key := fmt.Sprintf("cache/fetch-manifest/%s", imageRef)
	_ = onMemoryCacheForVerifyResource.Set(key, concatYAMLbytes, err)
}

type AnnotationManifestFetcher struct {
}

func (f *AnnotationManifestFetcher) Fetch(objYAMLBytes []byte) ([]byte, string, error) {
	// TODO: support annotation signature
	return nil, "", errors.New("annotation-embedded signature is not supported yet")
}

type VerifyResult struct {
	Verified bool                `json:"verified"`
	Signer   string              `json:"signer"`
	Diff     *mapnode.DiffResult `json:"diff"`
}

func (r *VerifyResult) String() string {
	rB, _ := json.Marshal(r)
	return string(rB)
}
