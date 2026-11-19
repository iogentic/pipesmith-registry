package auth

import (
	"strings"

	"github.com/iogentic/pipesmith-registry/internal/model"
)

// Decision N12 of the phase 3 plan: a Docker repository asks for Basic when
// it forces Basic sign in, which its stored attribute docker.forceBasicAuth
// says, otherwise for a bearer token of the token route while the realm of
// Docker tokens is active, otherwise for Basic. A pull without credentials
// needs that realm, anonymous access and the attribute, all three (lab
// research 05-identity.md, section Anonymous). A repository without the
// attribute, or one the snapshot does not hold, forces Basic.

// DockerAccess is the sign in of a Docker repository for the challenge and
// for an anonymous pull.
func (s *Service) DockerAccess(repository string) model.DockerAccess {
	return s.view().dockerAccess(repository)
}

func (v *view) dockerAccess(repository string) model.DockerAccess {
	bearer := !v.forcesBasic(repository) && v.realms[RealmDocker]
	return model.DockerAccess{Bearer: bearer, AnonymousPull: bearer && v.snap.Anonymous.Enabled}
}

// forcesBasic reads docker.forceBasicAuth of the stored repository.
func (v *view) forcesBasic(repository string) bool {
	r, ok := v.repos[repository]
	if !ok {
		return true
	}
	docker, _ := r.Attributes["docker"].(map[string]any)
	force, ok := docker["forceBasicAuth"].(bool)
	return !ok || force
}

// dockerPull is the Docker repository of a view permission of the read path
// of the registry, any action but browse: the anonymous subject holds it only
// under the three conditions.
func dockerPull(permission string) (string, bool) {
	parts := strings.Split(permission, ":")
	if len(parts) != 5 || parts[1] != "repository-view" || parts[2] != "docker" || parts[4] == "browse" {
		return "", false
	}
	return parts[3], true
}
