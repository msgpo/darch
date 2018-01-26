package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/opencontainers/image-spec/identity"

	"github.com/containerd/containerd"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/diff"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/oci"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pauldotknopf/darch/pkg/recipes"
	"github.com/pauldotknopf/darch/pkg/reference"
	"github.com/pauldotknopf/darch/pkg/utils"
	"github.com/pauldotknopf/darch/pkg/workspace"
)

const containerdUncompressed = "containerd.io/uncompressed"

// BuildRecipe Builds a recipe.
func (session *Session) BuildRecipe(ctx context.Context, recipe recipes.Recipe, tag string, imagePrefix string, env []string) (reference.ImageRef, error) {

	ctx = namespaces.WithNamespace(ctx, "darch")

	if len(tag) == 0 {
		tag = "latest"
	}

	newImage, err := reference.ParseImage(imagePrefix + recipe.Name + ":" + tag)
	if err != nil {
		return reference.ImageRef{}, err
	}

	// Use the image prefix when inheriting local recipes.
	// External references are expected to be fully qualified.
	inherits := recipe.Inherits
	if !recipe.InheritsExternal {
		fmt.Printf("Not going external: %s\n", inherits)
		inherits = imagePrefix + inherits
		fmt.Println("--" + inherits)
	}

	// NOTE: We use ParseImageWithDefaultTag here.
	// This allows recipes to use specific tags, but when
	// they aren't, it uses the tag the we are building
	// the recipe with.
	// This allows use to "darch build -t custom-tag base base-common"
	// and each built image will use the appropriate inherited image.
	inheritsRef, err := reference.ParseImageWithDefaultTag(inherits, newImage.Tag)
	if err != nil {
		return newImage, err
	}

	img, err := session.client.GetImage(ctx, inheritsRef.FullName())
	if err != nil {
		return newImage, err
	}

	ws, err := workspace.NewWorkspace("/tmp")
	if err != nil {
		return newImage, err
	}
	defer ws.Destroy()

	mounts, err := createTempMounts(ws.Path)

	mounts = append(mounts, specs.Mount{
		Destination: "/recipes",
		Type:        "bind",
		Source:      recipe.RecipesDir,
		Options:     []string{"rbind", "ro"},
	})

	// Let's create the snapshot that all of our containers will run off of
	snapshotKey := utils.NewID()
	err = session.createSnapshot(ctx, snapshotKey, img)
	if err != nil {
		return newImage, err
	}
	defer session.deleteSnapshot(ctx, snapshotKey)

	if err = session.RunContainer(ctx, ContainerConfig{
		newOpts: []containerd.NewContainerOpts{
			containerd.WithImage(img),
			containerd.WithSnapshotter(containerd.DefaultSnapshotter),
			containerd.WithSnapshot(snapshotKey),
			containerd.WithRuntime(fmt.Sprintf("io.containerd.runtime.v1.%s", runtime.GOOS), nil),
			containerd.WithNewSpec(
				oci.WithImageConfig(img),
				oci.WithEnv(env),
				oci.WithHostNamespace(specs.NetworkNamespace),
				oci.WithMounts(mounts),
				oci.WithProcessArgs("/usr/bin/env", "bash", "-c", "/darch-prepare"),
			),
		},
	}); err != nil {
		return newImage, err
	}

	if err = session.RunContainer(ctx, ContainerConfig{
		newOpts: []containerd.NewContainerOpts{
			containerd.WithImage(img),
			containerd.WithSnapshotter(containerd.DefaultSnapshotter),
			containerd.WithSnapshot(snapshotKey),
			containerd.WithRuntime(fmt.Sprintf("io.containerd.runtime.v1.%s", runtime.GOOS), nil),
			containerd.WithNewSpec(
				oci.WithImageConfig(img),
				oci.WithEnv(env),
				oci.WithHostNamespace(specs.NetworkNamespace),
				oci.WithMounts(mounts),
				oci.WithProcessArgs("/usr/bin/env", "bash", "-c", fmt.Sprintf("/darch-runrecipe %s", recipe.Name)),
			),
		},
	}); err != nil {
		return newImage, err
	}

	if err = session.RunContainer(ctx, ContainerConfig{
		newOpts: []containerd.NewContainerOpts{
			containerd.WithImage(img),
			containerd.WithSnapshotter(containerd.DefaultSnapshotter),
			containerd.WithSnapshot(snapshotKey),
			containerd.WithRuntime(fmt.Sprintf("io.containerd.runtime.v1.%s", runtime.GOOS), nil),
			containerd.WithNewSpec(
				oci.WithImageConfig(img),
				oci.WithEnv(env),
				oci.WithHostNamespace(specs.NetworkNamespace),
				oci.WithMounts(mounts),
				oci.WithProcessArgs("/usr/bin/env", "bash", "-c", "/darch-teardown"),
			),
		},
	}); err != nil {
		return newImage, err
	}

	return newImage, session.createImageFromSnapshot(ctx, img, snapshotKey, newImage)
}

func (session *Session) createSnapshot(ctx context.Context, snapshotKey string, img containerd.Image) error {
	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return err
	}
	parent := identity.ChainID(diffIDs).String()
	if _, err := session.client.SnapshotService(containerd.DefaultSnapshotter).Prepare(ctx, snapshotKey, parent); err != nil {
		return err
	}
	return nil
}

func (session *Session) deleteSnapshot(ctx context.Context, snapshotKey string) error {
	return session.client.SnapshotService(containerd.DefaultSnapshotter).Remove(ctx, snapshotKey)
}

func (session *Session) patchImageConfig(ctx context.Context, ref string, manifest *ocispec.Manifest, newLayerDigest digest.Digest) error {
	// Get the current image configuration.
	p, err := content.ReadBlob(ctx, session.client.ContentStore(), manifest.Config.Digest)
	if err != nil {
		return err
	}

	// Deserialize the image configuration to a generic json object.
	// We do this so that we can patch it, without requiring knowledge
	// of the entire schema.
	m := map[string]json.RawMessage{}
	if err = json.Unmarshal(p, &m); err != nil {
		return err
	}

	// Pull the rootfs section out, so that we can append a layer to the diff_ids array.
	var rootFS ocispec.RootFS
	p, err = m["rootfs"].MarshalJSON()
	if err != nil {
		return err
	}
	if err = json.Unmarshal(p, &rootFS); err != nil {
		return err
	}
	rootFS.DiffIDs = append(rootFS.DiffIDs, newLayerDigest)
	p, err = json.Marshal(rootFS)
	if err != nil {
		return err
	}
	m["rootfs"] = p

	// Convert our entire image configuration back to bytes, and write it to the content store.
	p, err = json.Marshal(m)
	if err != nil {
		return err
	}
	manifest.Config.Digest = digest.FromBytes(p)
	manifest.Config.Size = int64(len(p))
	err = content.WriteBlob(ctx, session.client.ContentStore(),
		ref,
		bytes.NewReader(p),
		manifest.Config.Size,
		manifest.Config.Digest,
	)
	if err != nil {
		return err
	}

	return err
}

func (session *Session) createImageFromSnapshot(ctx context.Context, img containerd.Image, activeSnapshotKey string, newImage reference.ImageRef) error {
	ctx, done, err := session.client.WithLease(ctx) // Prevent garbage collection while we work.
	if err != nil {
		return err
	}
	defer done()

	contentStore := session.client.ContentStore()
	snapshotService := session.client.SnapshotService(containerd.DefaultSnapshotter)
	imgTarget := img.Target()

	// First, let's get the parent image digest, so that we can
	// later create a new one from it, with a new layer added to it.
	p, err := content.ReadBlob(ctx, contentStore, imgTarget.Digest)
	if err != nil {
		return err
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(p, &manifest); err != nil {
		return err
	}

	snapshot, err := snapshotService.Stat(ctx, activeSnapshotKey)
	if err != nil {
		return err
	}

	upperMounts, err := snapshotService.Mounts(ctx, activeSnapshotKey)
	if err != nil {
		return err
	}

	lowerMounts, err := snapshotService.View(ctx, "temp-readonly-parent", snapshot.Parent)
	if err != nil {
		return err
	}
	defer snapshotService.Remove(ctx, "temp-readonly-parent")

	// Generate a diff in content store
	diffs, err := session.client.DiffService().DiffMounts(ctx,
		lowerMounts,
		upperMounts,
		diff.WithMediaType(ocispec.MediaTypeImageLayerGzip),
		diff.WithReference("custom-ref"))
	if err != nil {
		return err
	}

	// These builds can be done on docker images, or OCI image.
	// Let's make sure the new layer uses the same content type as the manifest expects.
	switch imgTarget.MediaType {
	case images.MediaTypeDockerSchema2Manifest:
		diffs.MediaType = images.MediaTypeDockerSchema2LayerGzip
		break
	case ocispec.MediaTypeImageManifest:
		diffs.MediaType = ocispec.MediaTypeImageLayerGzip
		break
	default:
		return fmt.Errorf("unknown parent image manifest type: %s", imgTarget.MediaType)
	}

	// Add our new layer to the image manifest
	manifest.Layers = append(manifest.Layers, diffs)

	// Add the blob checksum to image config
	info, err := contentStore.Info(ctx, diffs.Digest)
	if err != nil {
		return err
	}
	diffIDStr, ok := info.Labels[containerdUncompressed]
	if !ok {
		return fmt.Errorf("invalid differ response with no diffID")
	}
	diffIDDigest, err := digest.Parse(diffIDStr)
	if err != nil {
		return err
	}
	err = session.patchImageConfig(ctx, "custom-ref", &manifest, diffIDDigest)
	if err != nil {
		return err
	}

	// Prepare the labels that will tell the garbage collector
	// to NOT delete the content this manifest references.
	labels := map[string]string{
		"containerd.io/gc.ref.content.0": manifest.Config.Digest.String(),
	}
	for i, layer := range manifest.Layers {
		labels[fmt.Sprintf("containerd.io/gc.ref.content.%d", i+1)] = layer.Digest.String()
	}

	// Save our new image manifest, which now hows our new layer,
	// and a patched image config with a reference to the new layer.
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	manifestDigest := digest.FromBytes(manifestBytes)
	if err := content.WriteBlob(ctx,
		contentStore,
		"custom-ref",
		bytes.NewReader(manifestBytes),
		int64(len(manifestBytes)),
		manifestDigest,
		content.WithLabels(labels)); err != nil {
		return err
	}

	// Let's see if the image exists already, if so, let's delete it
	_, err = session.client.GetImage(ctx, newImage.FullName())
	if err == nil {
		session.client.ImageService().Delete(ctx, newImage.FullName(), images.SynchronousDelete())
	}

	_, err = session.client.ImageService().Create(ctx,
		images.Image{
			Name: newImage.FullName(),
			Target: ocispec.Descriptor{
				Digest:    manifestDigest,
				Size:      int64(len(manifestBytes)),
				MediaType: imgTarget.MediaType, /*use same one as inherited image*/
			},
		})
	if err != nil {
		return err
	}

	// This will create the required snapshot for the new layer,
	// which will allow us to run the image immediately.
	imageBuilt, err := session.client.GetImage(ctx, newImage.FullName())
	if err != nil {
		return err
	}
	err = imageBuilt.Unpack(ctx, containerd.DefaultSnapshotter)
	if err != nil {
		return err
	}

	return nil
}
