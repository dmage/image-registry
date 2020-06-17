package imagestream

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	dcontext "github.com/docker/distribution/context"
	"github.com/opencontainers/go-digest"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	imageapiv1 "github.com/openshift/api/image/v1"

	"github.com/openshift/image-registry/pkg/dockerregistry/server/client"
	rerrors "github.com/openshift/image-registry/pkg/errors"
	imageapi "github.com/openshift/image-registry/pkg/origin-common/image/apis/image"
	quotautil "github.com/openshift/image-registry/pkg/origin-common/quota/util"
	originutil "github.com/openshift/image-registry/pkg/origin-common/util"
)

const (
	ErrImageStreamCode              = "ImageStream:"
	ErrImageStreamUnknownErrorCode  = ErrImageStreamCode + "Unknown"
	ErrImageStreamNotFoundCode      = ErrImageStreamCode + "NotFound"
	ErrImageStreamImageNotFoundCode = ErrImageStreamCode + "ImageNotFound"
	ErrImageStreamForbiddenCode     = ErrImageStreamCode + "Forbidden"
)

// ProjectObjectListStore represents a cache of objects indexed by a project name.
// Used to store a list of items per namespace.
type ProjectObjectListStore interface {
	Add(namespace string, obj runtime.Object) error
	Get(namespace string) (obj runtime.Object, exists bool, err error)
}

type ImageStream interface {
	Reference() string
	Clone(namespace, name string) ImageStream
	Exists(ctx context.Context) (bool, rerrors.Error)

	GetImageOfImageStream(ctx context.Context, dgst digest.Digest) (*imageapiv1.Image, rerrors.Error)
	CreateImageStreamMapping(ctx context.Context, userClient client.Interface, tag string, image *imageapiv1.Image) rerrors.Error
	ResolveImageID(ctx context.Context, dgst digest.Digest) (*imageapiv1.TagEvent, rerrors.Error)

	HasBlob(ctx context.Context, dgst digest.Digest) (bool, *imageapiv1.ImageStreamLayers, *imageapiv1.Image)
	RemoteRepositoriesForBlob(ctx context.Context, dgst digest.Digest) ([]RemoteRepository, []ImageStreamReference, rerrors.Error)
	RemoteRepositoriesForManifest(ctx context.Context, dgst digest.Digest) ([]RemoteRepository, []ImageStreamReference, rerrors.Error)
	GetLimitRangeList(ctx context.Context, cache ProjectObjectListStore) (*corev1.LimitRangeList, rerrors.Error)
	GetSecrets() ([]corev1.Secret, rerrors.Error)

	TagIsInsecure(ctx context.Context, tag string, dgst digest.Digest) (bool, rerrors.Error)
	Tags(ctx context.Context) (map[string]digest.Digest, rerrors.Error)
}

type imageStream struct {
	namespace string
	name      string

	registryOSClient client.Interface

	imageClient imageGetter

	// imageStreamGetter fetches and caches an image stream. The image stream stays cached for the entire time of handling single repository-scoped request.
	imageStreamGetter *cachedImageStreamGetter
}

var _ ImageStream = &imageStream{}

func New(namespace, name string, client client.Interface) ImageStream {
	return &imageStream{
		namespace:        namespace,
		name:             name,
		registryOSClient: client,
		imageClient:      newCachedImageGetter(client),
		imageStreamGetter: &cachedImageStreamGetter{
			namespace:    namespace,
			name:         name,
			isNamespacer: client,
		},
	}
}

func (is *imageStream) Reference() string {
	return fmt.Sprintf("%s/%s", is.namespace, is.name)
}

func (is *imageStream) Clone(namespace, name string) ImageStream {
	if is.namespace == namespace && is.name == name {
		return is
	}
	return &imageStream{
		namespace:        namespace,
		name:             name,
		registryOSClient: is.registryOSClient,
		imageClient:      newCachedImageGetter(is.registryOSClient),
		imageStreamGetter: &cachedImageStreamGetter{
			namespace:    namespace,
			name:         name,
			isNamespacer: is.registryOSClient,
		},
	}
}

// getImage retrieves the Image with digest `dgst`. No authorization check is done.
func (is *imageStream) getImage(ctx context.Context, dgst digest.Digest) (*imageapiv1.Image, rerrors.Error) {
	image, err := is.imageClient.Get(ctx, dgst)

	switch {
	case kerrors.IsNotFound(err):
		return nil, rerrors.NewError(
			ErrImageStreamImageNotFoundCode,
			fmt.Sprintf("getImage: unable to find image digest %s in %s", dgst.String(), is.name),
			err,
		)
	case err != nil:
		return nil, rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("getImage: unable to get image digest %s in %s", dgst.String(), is.name),
			err,
		)
	}

	return image, nil
}

// ResolveImageID returns latest TagEvent for specified imageID and an error if
// there's more than one image matching the ID or when one does not exist.
func (is *imageStream) ResolveImageID(ctx context.Context, dgst digest.Digest) (*imageapiv1.TagEvent, rerrors.Error) {
	stream, rErr := is.imageStreamGetter.get()

	if rErr != nil {
		return nil, convertImageStreamGetterError(rErr, fmt.Sprintf("ResolveImageID: failed to get image stream %s", is.Reference()))
	}

	tagEvent, err := originutil.ResolveImageID(stream, dgst.String())
	if err != nil {
		code := ErrImageStreamUnknownErrorCode

		if kerrors.IsNotFound(err) {
			code = ErrImageStreamImageNotFoundCode
		}

		return nil, rerrors.NewError(
			code,
			fmt.Sprintf("ResolveImageID: unable to resolve ImageID %s in image stream %s", dgst.String(), is.Reference()),
			err,
		)
	}

	return tagEvent, nil
}

// GetStoredImageOfImageStream retrieves the Image with digest `dgst` and
// ensures that the image belongs to the image stream `is`. It uses two
// queries to master API:
//
//  1st to get a corresponding image stream
//  2nd to get the image
//
// This allows us to cache the image stream for later use.
//
// If you need the image object to be modified according to image stream tag,
// please use GetImageOfImageStream.
func (is *imageStream) getStoredImageOfImageStream(ctx context.Context, dgst digest.Digest) (*imageapiv1.Image, *imageapiv1.TagEvent, rerrors.Error) {
	tagEvent, err := is.ResolveImageID(ctx, dgst)
	if err != nil {
		return nil, nil, err
	}

	image, err := is.getImage(ctx, dgst)
	if err != nil {
		return nil, nil, err
	}

	return image, tagEvent, nil
}

// GetImageOfImageStream retrieves the Image with digest `dgst` for the image
// stream. The image's field DockerImageReference is modified on the fly to
// pretend that we've got the image from the source from which the image was
// tagged to match tag's DockerImageReference.
//
// NOTE: due to on the fly modification, the returned image object should
// not be sent to the master API. If you need unmodified version of the
// image object, please use getStoredImageOfImageStream.
func (is *imageStream) GetImageOfImageStream(ctx context.Context, dgst digest.Digest) (*imageapiv1.Image, rerrors.Error) {
	image, tagEvent, err := is.getStoredImageOfImageStream(ctx, dgst)
	if err != nil {
		return nil, err
	}

	// We don't want to mutate the origial image object, which we've got by reference.
	img := *image
	img.DockerImageReference = tagEvent.DockerImageReference

	return &img, nil
}

func (is *imageStream) GetSecrets() ([]corev1.Secret, rerrors.Error) {
	secrets, err := is.registryOSClient.ImageStreamSecrets(is.namespace).Secrets(context.TODO(), is.name, metav1.GetOptions{})
	if err != nil {
		return nil, rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("GetSecrets: error getting secrets for repository %s", is.Reference()),
			err,
		)
	}
	return secrets.Items, nil
}

// TagIsInsecure returns true if the given image stream or its tag allow for
// insecure transport.
func (is *imageStream) TagIsInsecure(ctx context.Context, tag string, dgst digest.Digest) (bool, rerrors.Error) {
	stream, err := is.imageStreamGetter.get()

	if err != nil {
		return false, convertImageStreamGetterError(err, fmt.Sprintf("TagIsInsecure: failed to get image stream %s", is.Reference()))
	}

	if insecure, _ := stream.Annotations[imageapi.InsecureRepositoryAnnotation]; insecure == "true" {
		return true, nil
	}

	if len(tag) == 0 {
		// if the client pulled by digest, find the corresponding tag in the image stream
		tag, _ = originutil.LatestImageTagEvent(stream, dgst.String())
	}

	if len(tag) != 0 {
		for _, t := range stream.Spec.Tags {
			if t.Name == tag {
				return t.ImportPolicy.Insecure, nil
			}
		}
	}

	return false, nil
}

func (is *imageStream) Exists(ctx context.Context) (bool, rerrors.Error) {
	_, rErr := is.imageStreamGetter.get()
	if rErr != nil {
		if rErr.Code() == ErrImageStreamGetterNotFoundCode {
			return false, nil
		}
		return false, convertImageStreamGetterError(rErr, fmt.Sprintf("Exists: failed to get image stream %s", is.Reference()))
	}
	return true, nil
}

func getLocalRegistryNames(ctx context.Context, stream *imageapiv1.ImageStream) []string {
	var localNames []string

	local, err := imageapi.ParseDockerImageReference(stream.Status.DockerImageRepository)
	if err != nil {
		dcontext.GetLogger(ctx).Warnf("getLocalRegistryNames: unable to parse dockerImageRepository %q: %v", stream.Status.DockerImageRepository, err)
	}
	if len(local.Registry) != 0 {
		localNames = append(localNames, local.Registry)
	}

	if len(stream.Status.PublicDockerImageRepository) != 0 {
		public, err := imageapi.ParseDockerImageReference(stream.Status.PublicDockerImageRepository)
		if err != nil {
			dcontext.GetLogger(ctx).Warnf("getLocalRegistryNames: unable to parse publicDockerImageRepository %q: %v", stream.Status.PublicDockerImageRepository, err)
		}
		if len(public.Registry) != 0 {
			localNames = append(localNames, public.Registry)
		}
	}

	return localNames
}

func imageStreamHasExternalReferences(ctx context.Context, stream *imageapiv1.ImageStream) bool {
	localRegistry := getLocalRegistryNames(ctx, stream)
	var localPrefixes []string
	for _, registry := range localRegistry {
		localPrefixes = append(localPrefixes, fmt.Sprintf("%s/%s/%s@", registry, stream.Namespace, stream.Name))
	}

	for _, tag := range stream.Status.Tags {
		for _, item := range tag.Items {
			local := false
			for _, p := range localPrefixes {
				if strings.HasPrefix(item.DockerImageReference, p) {
					local = true
					break
				}
			}
			if !local {
				return true
			}
		}
	}
	return false
}

func imageBlobReferencesHasBlob(info imageapiv1.ImageBlobReferences, dgst digest.Digest) bool {
	if info.Config != nil && *info.Config == dgst.String() {
		return true
	}
	for _, layer := range info.Layers {
		if layer == dgst.String() {
			return true
		}
	}
	return false
}

// RemoteRepositoriesForBlob returns a list of repositories that are imported
// into the image stream and may have the blob dgst. For the repositories that
// are hosted by the local registry, image stream references will be returned
// instead. The repository is assumed to have the blob if its manifests use the
// blob.
func (is *imageStream) RemoteRepositoriesForBlob(ctx context.Context, dgst digest.Digest) ([]RemoteRepository, []ImageStreamReference, rerrors.Error) {
	stream, err := is.imageStreamGetter.get()
	if err != nil {
		return nil, nil, convertImageStreamGetterError(err, fmt.Sprintf("RemoteRepositoriesForBlob: failed to get image stream %s", is.Reference()))
	}

	if !imageStreamHasExternalReferences(ctx, stream) {
		// We don't need to check layers as the image stream doesn't have
		// external references anyway.
		return nil, nil, nil
	}

	layers, err := is.imageStreamGetter.layers()
	if err != nil {
		return nil, nil, convertImageStreamGetterError(err, fmt.Sprintf("RemoteRepositoriesForBlob: failed to get image stream layers %s", is.Reference()))
	}

	dcontext.GetLogger(ctx).Debugf("RemoteRepositoriesForBlob: got %s layers: %#+v", is.Reference(), layers)

	var images []string
	for image, info := range layers.Images {
		if imageBlobReferencesHasBlob(info, dgst) {
			images = append(images, image)
		}
	}

	if len(images) == 0 {
		dcontext.GetLogger(ctx).Debugf("RemoteRepositoriesForBlob: no images found in %s with blob %s", is.Reference(), dgst)
		return nil, nil, nil
	}

	dcontext.GetLogger(ctx).Debugf("RemoteRepositoriesForBlob: found images in %s with blob %s: %v", is.Reference(), dgst, images)

	repos, isrefs := remoteRepositoriesForImages(ctx, stream, images)

	dcontext.GetLogger(ctx).Debugf("RemoteRepositoriesForBlob: repositories from imagestream %s for blob %s: repos=%+v isrefs=%+v", is.Reference(), dgst, repos, isrefs)

	return repos, isrefs, nil
}

// RemoteRepositoriesForManifest returns a list of repositories that are
// imported into the image stream and may have the manifest dgst. For the
// repositories that are hosted by the local registry, image stream references
// will be returned instead.
func (is *imageStream) RemoteRepositoriesForManifest(ctx context.Context, dgst digest.Digest) ([]RemoteRepository, []ImageStreamReference, rerrors.Error) {
	stream, err := is.imageStreamGetter.get()
	if err != nil {
		return nil, nil, convertImageStreamGetterError(err, fmt.Sprintf("RemoteRepositoriesForManifest: failed to get image stream %s", is.Reference()))
	}

	repos, isrefs := remoteRepositoriesForImages(ctx, stream, []string{dgst.String()})

	dcontext.GetLogger(ctx).Debugf("RemoteRepositoriesForManifest: repositories from imagestream %s for manifest %s: repos=%+v isrefs=%+v", is.Reference(), dgst, repos, isrefs)

	return repos, isrefs, nil
}

func (is *imageStream) Tags(ctx context.Context) (map[string]digest.Digest, rerrors.Error) {
	stream, err := is.imageStreamGetter.get()
	if err != nil {
		return nil, convertImageStreamGetterError(err, fmt.Sprintf("Tags: failed to get image stream %s", is.Reference()))
	}

	m := make(map[string]digest.Digest)

	for _, history := range stream.Status.Tags {
		if len(history.Items) == 0 {
			continue
		}

		tag := history.Tag

		dgst, err := digest.Parse(history.Items[0].Image)
		if err != nil {
			dcontext.GetLogger(ctx).Errorf("bad digest %s: %v", history.Items[0].Image, err)
			continue
		}

		m[tag] = dgst
	}

	return m, nil
}

func (is *imageStream) CreateImageStreamMapping(ctx context.Context, userClient client.Interface, tag string, image *imageapiv1.Image) rerrors.Error {
	ism := imageapiv1.ImageStreamMapping{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: is.namespace,
			Name:      is.name,
		},
		Image: *image,
		Tag:   tag,
	}

	_, err := is.registryOSClient.ImageStreamMappings(is.namespace).Create(ctx, &ism, metav1.CreateOptions{})

	if err == nil {
		return nil
	}

	if quotautil.IsErrorQuotaExceeded(err) {
		return rerrors.NewError(
			ErrImageStreamForbiddenCode,
			fmt.Sprintf("CreateImageStreamMapping: quota exceeded during creation of %s ImageStreamMapping", is.Reference()),
			err,
		)
	}

	// if the error was that the image stream wasn't found, try to auto provision it
	statusErr, ok := err.(*kerrors.StatusError)
	if !ok {
		return rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("CreateImageStreamMapping: error creating %s ImageStreamMapping", is.Reference()),
			err,
		)
	}

	status := statusErr.ErrStatus
	isValidKind := false

	if kerrors.IsNotFound(statusErr) && strings.ToLower(status.Details.Kind) == "namespaces" {
		return rerrors.NewError(
			ErrImageStreamForbiddenCode,
			fmt.Sprintf("CreateImageStreamMapping: error creating %s ImageStreamMapping", is.Reference()),
			err,
		)
	}

	if status.Details != nil && status.Details.Kind == "imagestreammappings" {
		isValidKind = true
	}
	if !isValidKind || status.Code != http.StatusNotFound || status.Details.Name != is.name {
		return rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("CreateImageStreamMapping: error creation of %s ImageStreamMapping", is.Reference()),
			err,
		)
	}

	stream := &imageapiv1.ImageStream{}
	stream.Name = is.name

	_, err = userClient.ImageStreams(is.namespace).Create(ctx, stream, metav1.CreateOptions{})

	switch {
	case kerrors.IsAlreadyExists(err), kerrors.IsConflict(err):
		// It is ok.
	case kerrors.IsForbidden(err), kerrors.IsUnauthorized(err), quotautil.IsErrorQuotaExceeded(err):
		return rerrors.NewError(
			ErrImageStreamForbiddenCode,
			fmt.Sprintf("CreateImageStreamMapping: denied creating ImageStream %s", is.Reference()),
			err,
		)
	case err != nil:
		return rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("CreateImageStreamMapping: error auto provisioning ImageStream %s", is.Reference()),
			err,
		)
	}

	dcontext.GetLogger(ctx).Debugf("cache image stream %s/%s", stream.Namespace, stream.Name)
	is.imageStreamGetter.cacheImageStream(stream)

	// try to create the ISM again
	_, err = is.registryOSClient.ImageStreamMappings(is.namespace).Create(ctx, &ism, metav1.CreateOptions{})

	if err == nil {
		return nil
	}

	if quotautil.IsErrorQuotaExceeded(err) {
		return rerrors.NewError(
			ErrImageStreamForbiddenCode,
			fmt.Sprintf("CreateImageStreamMapping: quota exceeded during creation of %s ImageStreamMapping second time", is.Reference()),
			err,
		)
	}

	return rerrors.NewError(
		ErrImageStreamUnknownErrorCode,
		fmt.Sprintf("CreateImageStreamMapping: error creating %s ImageStreamMapping second time", is.Reference()),
		err,
	)
}

// GetLimitRangeList returns list of limit ranges for repo.
func (is *imageStream) GetLimitRangeList(ctx context.Context, cache ProjectObjectListStore) (*corev1.LimitRangeList, rerrors.Error) {
	if cache != nil {
		obj, exists, _ := cache.Get(is.namespace)
		if exists {
			return obj.(*corev1.LimitRangeList), nil
		}
	}

	dcontext.GetLogger(ctx).Debugf("listing limit ranges in namespace %s", is.namespace)

	lrs, err := is.registryOSClient.LimitRanges(is.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, rerrors.NewError(
			ErrImageStreamUnknownErrorCode,
			fmt.Sprintf("GetLimitRangeList: failed to list limitranges for %s", is.Reference()),
			err,
		)
	}

	if cache != nil {
		err = cache.Add(is.namespace, lrs)
		if err != nil {
			dcontext.GetLogger(ctx).Errorf("GetLimitRangeList: failed to cache limit range list: %v", err)
		}
	}

	return lrs, nil
}

func convertImageStreamGetterError(err rerrors.Error, msg string) rerrors.Error {
	code := ErrImageStreamUnknownErrorCode

	switch err.Code() {
	case ErrImageStreamGetterNotFoundCode:
		code = ErrImageStreamNotFoundCode
	case ErrImageStreamGetterForbiddenCode:
		code = ErrImageStreamForbiddenCode
	}

	return rerrors.NewError(code, msg, err)
}
