package upstreammirror

import "github.com/ben-wangz/registry-hub-tests/internal/testkit"

type mirrorPhase struct {
	name string
	run  func() (map[string]any, error)
}

func runMirrorPhase(environment testkit.CaseContext, phase mirrorPhase) (map[string]any, error) {
	var result map[string]any
	err := testkit.RunPhase(environment, phase.name, func() error {
		var err error
		result, err = phase.run()
		return err
	})
	return result, err
}
