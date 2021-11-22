package cnabtooci

import (
	"context"
	"fmt"

	"strings"

	"github.com/cnabio/cnab-go/bundle"
	"github.com/cnabio/cnab-to-oci/relocation"
	"github.com/cnabio/cnab-to-oci/remotes"
	containerdRemotes "github.com/containerd/containerd/remotes"
	"github.com/docker/cli/cli/command"
	dockerconfig "github.com/docker/cli/cli/config"
	cliflags "github.com/docker/cli/cli/flags"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/term"
	"github.com/docker/docker/registry"
	"github.com/pkg/errors"

	portercontext "get.porter.sh/porter/pkg/context"
)

// ErrNoContentDigest represents an error due to an image not having a
// corresponding content digest in a bundle definition
type ErrNoContentDigest error

// NewErrNoContentDigest returns an ErrNoContentDigest formatted with the
// provided image name
func NewErrNoContentDigest(image string) ErrNoContentDigest {
	return fmt.Errorf("unable to verify that the pulled image %s is the invocation image referenced by the bundle because the bundle does not specify a content digest. This could allow for the invocation image to be replaced or tampered with", image)
}

var _ RegistryProvider = &Registry{}

type Registry struct {
	*portercontext.Context
}

func NewRegistry(c *portercontext.Context) *Registry {
	return &Registry{
		Context: c,
	}
}

// PullBundle pulls a bundle from an OCI registry. Returns the bundle, and an optional image relocation mapping, if applicable.
func (r *Registry) PullBundle(tag string, insecureRegistry bool) (bundle.Bundle, *relocation.ImageRelocationMap, error) {
	ref, err := reference.ParseNormalizedNamed(tag)
	if err != nil {
		return bundle.Bundle{}, nil, errors.Wrap(err, "invalid bundle tag format, expected REGISTRY/name:tag")
	}

	var insecureRegistries []string
	if insecureRegistry {
		reg := reference.Domain(ref)
		insecureRegistries = append(insecureRegistries, reg)
	}

	if r.Debug {
		msg := strings.Builder{}
		msg.WriteString("Pulling bundle ")
		msg.WriteString(ref.String())
		if insecureRegistry {
			msg.WriteString(" with --insecure-registry")
		}
		fmt.Fprintln(r.Err, msg.String())
	}

	bun, reloMap, err := remotes.Pull(context.Background(), ref, r.createResolver(insecureRegistries))
	if err != nil {
		return bundle.Bundle{}, nil, errors.Wrap(err, "unable to pull remote bundle")
	}

	invocationImage := bun.InvocationImages[0]
	if invocationImage.Digest == "" {
		return bundle.Bundle{}, nil,
			NewErrNoContentDigest(invocationImage.Image)
	}

	if len(reloMap) == 0 {
		return *bun, nil, nil
	}
	return *bun, &reloMap, nil
}

func (r *Registry) PushBundle(bun bundle.Bundle, tag string, reloMap relocation.ImageRelocationMap, insecureRegistry bool) (*relocation.ImageRelocationMap, error) {
	ref, err := ParseOCIReference(tag) //tag from manifest
	if err != nil {
		return nil, errors.Wrap(err, "invalid bundle tag reference. expected value is REGISTRY/bundle:tag")
	}
	var insecureRegistries []string
	if insecureRegistry {
		reg := reference.Domain(ref)
		insecureRegistries = append(insecureRegistries, reg)
	}

	resolver := r.createResolver(insecureRegistries)

	// Initialize the relocation map if necessary
	if reloMap == nil {
		reloMap = make(relocation.ImageRelocationMap)
	}
	rm, err := remotes.FixupBundle(context.Background(), &bun, ref, resolver, remotes.WithEventCallback(r.displayEvent), remotes.WithAutoBundleUpdate(), remotes.WithRelocationMap(reloMap))
	if err != nil {
		return nil, errors.Wrap(err, "error preparing the bundle with cnab-to-oci before pushing")
	}
	d, err := remotes.Push(context.Background(), &bun, rm, ref, resolver, true)
	if err != nil {
		return nil, errors.Wrapf(err, "error pushing the bundle to %s", tag)
	}
	fmt.Fprintf(r.Out, "Bundle tag %s pushed successfully, with digest %q\n", ref, d.Digest)

	if len(rm) == 0 {
		return nil, nil
	}
	return &rm, nil
}

// PushInvocationImage pushes the invocation image from the Docker image cache to the specified location
// the expected format of the invocationImage is REGISTRY/NAME:TAG.
// Returns the image digest from the registry.
func (r *Registry) PushInvocationImage(invocationImage string) (string, error) {
	cli, err := r.getDockerClient()
	if err != nil {
		return "", err
	}

	ctx := context.Background()

	ref, err := ParseOCIReference(invocationImage)
	if err != nil {
		return "", err
	}
	// Resolve the Repository name from fqn to RepositoryInfo
	repoInfo, err := registry.ParseRepositoryInfo(ref)
	if err != nil {
		return "", err
	}
	authConfig := command.ResolveAuthConfig(ctx, cli, repoInfo.Index)
	encodedAuth, err := command.EncodeAuthToBase64(authConfig)
	if err != nil {
		return "", err
	}
	options := types.ImagePushOptions{
		All:          true,
		RegistryAuth: encodedAuth,
	}

	fmt.Fprintln(r.Out, "Pushing CNAB invocation image...")
	pushResponse, err := cli.Client().ImagePush(ctx, invocationImage, options)
	if err != nil {
		return "", errors.Wrap(err, "docker push failed")
	}
	defer pushResponse.Close()

	termFd, _ := term.GetFdInfo(r.Out)
	// Setting this to false here because Moby os.Exit(1) all over the place and this fails on WSL (only)
	// when Term is true.
	isTerm := false
	err = jsonmessage.DisplayJSONMessagesStream(pushResponse, r.Out, termFd, isTerm, nil)
	if err != nil {
		if strings.HasPrefix(err.Error(), "denied") {
			return "", errors.Wrap(err, "docker push authentication failed")
		}
		return "", errors.Wrap(err, "failed to stream docker push stdout")
	}
	dist, err := cli.Client().DistributionInspect(ctx, invocationImage, encodedAuth)
	if err != nil {
		return "", errors.Wrap(err, "unable to inspect docker image")
	}
	return string(dist.Descriptor.Digest), nil
}

func (r *Registry) createResolver(insecureRegistries []string) containerdRemotes.Resolver {
	return remotes.CreateResolver(dockerconfig.LoadDefaultConfigFile(r.Out), insecureRegistries...)
}

func (r *Registry) displayEvent(ev remotes.FixupEvent) {
	switch ev.EventType {
	case remotes.FixupEventTypeCopyImageStart:
		fmt.Fprintf(r.Out, "Starting to copy image %s...\n", ev.SourceImage)
	case remotes.FixupEventTypeCopyImageEnd:
		if ev.Error != nil {
			fmt.Fprintf(r.Out, "Failed to copy image %s: %s\n", ev.SourceImage, ev.Error)
		} else {
			fmt.Fprintf(r.Out, "Completed image %s copy\n", ev.SourceImage)
		}
	}
}

func (r *Registry) getDockerClient() (*command.DockerCli, error) {
	cli, err := command.NewDockerCli()
	if err != nil {
		return nil, errors.Wrap(err, "could not create new docker client")
	}
	if err := cli.Initialize(cliflags.NewClientOptions()); err != nil {
		return nil, err
	}
	return cli, nil
}

func ParseOCIReference(ociRef string) (reference.Named, error) {
	return reference.ParseNormalizedNamed(ociRef)
}
