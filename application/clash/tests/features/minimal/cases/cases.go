package cases

import (
	"encoding/json"
	connectionmetadata "github.com/ben-wangz/clash-tests/features/minimal/cases/connection-metadata"
	connections "github.com/ben-wangz/clash-tests/features/minimal/cases/connections"
	controllerauth "github.com/ben-wangz/clash-tests/features/minimal/cases/controller-auth"
	controllerrecovery "github.com/ben-wangz/clash-tests/features/minimal/cases/controller-recovery"
	defaultrender "github.com/ben-wangz/clash-tests/features/minimal/cases/default-render"
	ingressaccess "github.com/ben-wangz/clash-tests/features/minimal/cases/ingress-access"
	installation "github.com/ben-wangz/clash-tests/features/minimal/cases/installation"
	livelogs "github.com/ben-wangz/clash-tests/features/minimal/cases/live-logs"
	logbuffer "github.com/ben-wangz/clash-tests/features/minimal/cases/log-buffer"
	proxyinventory "github.com/ben-wangz/clash-tests/features/minimal/cases/proxy-inventory"
	proxyselection "github.com/ben-wangz/clash-tests/features/minimal/cases/proxy-selection"
	selectionerrors "github.com/ben-wangz/clash-tests/features/minimal/cases/selection-errors"
	streamrecovery "github.com/ben-wangz/clash-tests/features/minimal/cases/stream-recovery"
	suiteindependence "github.com/ben-wangz/clash-tests/features/minimal/cases/suite-independence"
	suitelifecycle "github.com/ben-wangz/clash-tests/features/minimal/cases/suite-lifecycle"
	traffic "github.com/ben-wangz/clash-tests/features/minimal/cases/traffic"
	"github.com/ben-wangz/clash-tests/internal/testkit"
)

func Definitions() ([]testkit.Definition, error) {
	var definitions []testkit.Definition
	for _, data := range [][]byte{
		defaultrender.Metadata,
		suitelifecycle.Metadata,
		suiteindependence.Metadata,
		installation.Metadata,
		proxyinventory.Metadata,
		proxyselection.Metadata,
		selectionerrors.Metadata,
		connections.Metadata,
		traffic.Metadata,
		connectionmetadata.Metadata,
		livelogs.Metadata,
		logbuffer.Metadata,
		streamrecovery.Metadata,
		controllerauth.Metadata,
		controllerrecovery.Metadata,
		ingressaccess.Metadata,
	} {
		var definition testkit.Definition
		if err := json.Unmarshal(data, &definition); err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}
