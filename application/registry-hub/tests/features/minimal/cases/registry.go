package cases

import (
	backends3 "github.com/ben-wangz/registry-hub-tests/features/backend-s3"
	clientauthcatalog "github.com/ben-wangz/registry-hub-tests/features/client-auth-catalog"
	"github.com/ben-wangz/registry-hub-tests/features/diagnostics"
	featuregc "github.com/ben-wangz/registry-hub-tests/features/gc"
	"github.com/ben-wangz/registry-hub-tests/features/minimal"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/admin-api"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/admin-auth"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/artifact-persistence"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/artifact-roundtrip"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/blob-mount"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/blob-read"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/blob-upload"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/client-interop"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/default-limits"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/default-render"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/delete-semantics"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/dependency-failure"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/external-combinations"
	imageinventory "github.com/ben-wangz/registry-hub-tests/features/minimal/cases/image-inventory"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/initialization-secrets"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/initializer-faults"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/installation"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/manifest-validation"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/observability"
	podmanpull "github.com/ben-wangz/registry-hub-tests/features/minimal/cases/podman-upstream-pull"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/rbac-boundary"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/referrers-lifecycle"
	referrersmigration "github.com/ben-wangz/registry-hub-tests/features/minimal/cases/referrers-migration"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/referrers-read"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/registry-protocol"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/registry-roundtrip"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/restart-reuse"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/routing-paths"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/security-defaults"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/startup-wait"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/tag-publication"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/tags-pagination"
	"github.com/ben-wangz/registry-hub-tests/features/minimal/cases/upstream-mirror"
	upstreamresume "github.com/ben-wangz/registry-hub-tests/features/minimal/cases/upstream-resume"
	referrersfallback "github.com/ben-wangz/registry-hub-tests/features/referrers-fallback"
	registrys3 "github.com/ben-wangz/registry-hub-tests/features/registry-s3"
	storageredirect "github.com/ben-wangz/registry-hub-tests/features/storage-redirect"
	upstreamauthtransport "github.com/ben-wangz/registry-hub-tests/features/upstream-auth-transport"
	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

func Registry() ([]testkit.Case, error) {
	constructors := []func() (testkit.Case, error){
		defaultrender.New,
		installation.New,
		initializationsecrets.New,
		initializerfaults.New,
		adminauth.New,
		externalcombinations.New,
		registryroundtrip.New,
		imageinventory.New,
		restartreuse.New,
		rbacboundary.New,
		startupwait.New,
		registryprotocol.New,
		routingpaths.New,
		blobupload.New,
		blobmount.New,
		blobread.New,
		manifestvalidation.New,
		tagpublication.New,
		tagspagination.New,
		artifactroundtrip.New,
		referrersread.New,
		referrersmigration.New,
		referrerslifecycle.New,
		deletesemantics.New,
		securitydefaults.New,
		adminapi.New,
		artifactpersistence.New,
		dependencyfailure.New,
		defaultlimits.New,
		observability.New,
		clientinterop.New,
		podmanpull.New,
		clientauthcatalog.New,
		upstreamauthtransport.New,
		backends3.New,
		diagnostics.New,
		registrys3.New,
		storageredirect.New,
		referrersfallback.New,
		featuregc.New,
		upstreammirror.New,
		upstreamresume.New,
	}
	registered := make([]testkit.Case, 0, len(constructors))
	for _, construct := range constructors {
		item, err := construct()
		if err != nil {
			return nil, err
		}
		registered = append(registered, item)
	}
	caseIDs := make(map[string]struct{}, len(registered))
	for _, item := range registered {
		caseIDs[item.Definition.ID] = struct{}{}
	}
	if err := minimal.ValidateCoverage(caseIDs); err != nil {
		return nil, err
	}
	return registered, nil
}
