package runner

import (
	"fmt"
	"sort"

	"github.com/ben-wangz/registry-hub-tests/internal/testkit"
)

type Plan struct {
	Explicit []string
	Added    []string
	Order    []string
	Edges    []DependencyEdge
}

type DependencyEdge struct {
	Dependency string
	Consumer   string
}

func BuildPlan(cases map[string]testkit.Case, selected []string) (Plan, error) {
	explicit := normalizeSelection(selected, cases)
	if len(explicit) == 0 {
		for id := range cases {
			explicit = append(explicit, id)
		}
		sort.Strings(explicit)
	}
	closure := make(map[string]struct{})
	var include func(string) error
	include = func(id string) error {
		item, exists := cases[id]
		if !exists {
			return fmt.Errorf("unknown case %q", id)
		}
		if _, included := closure[id]; included {
			return nil
		}
		closure[id] = struct{}{}
		for _, dependency := range item.Definition.DependsOn {
			if err := include(dependency); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range explicit {
		if _, exists := cases[id]; !exists {
			return Plan{}, fmt.Errorf("unknown case %q", id)
		}
		if err := include(id); err != nil {
			return Plan{}, err
		}
	}
	order, err := topologicalOrder(cases, closure)
	if err != nil {
		return Plan{}, err
	}
	automatic := make([]string, 0, len(closure)-len(explicit))
	edges := make([]DependencyEdge, 0)
	explicitSet := stringSet(explicit)
	for id := range closure {
		if _, exists := explicitSet[id]; !exists {
			automatic = append(automatic, id)
		}
		for _, dependency := range cases[id].Definition.DependsOn {
			if _, included := closure[dependency]; included {
				edges = append(edges, DependencyEdge{Dependency: dependency, Consumer: id})
			}
		}
	}
	sort.Strings(automatic)
	sort.Slice(edges, func(left, right int) bool {
		if edges[left].Dependency == edges[right].Dependency {
			return edges[left].Consumer < edges[right].Consumer
		}
		return edges[left].Dependency < edges[right].Dependency
	})
	return Plan{Explicit: explicit, Added: automatic, Order: order, Edges: edges}, nil
}

func normalizeSelection(selected []string, cases map[string]testkit.Case) []string {
	if len(selected) == 0 {
		return nil
	}
	unique := make(map[string]struct{}, len(selected))
	result := make([]string, 0, len(selected))
	for _, id := range selected {
		if _, exists := unique[id]; exists {
			continue
		}
		unique[id] = struct{}{}
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

func topologicalOrder(cases map[string]testkit.Case, closure map[string]struct{}) ([]string, error) {
	indegree := make(map[string]int, len(closure))
	consumers := make(map[string][]string, len(closure))
	for id := range closure {
		indegree[id] = 0
	}
	for id := range closure {
		for _, dependency := range cases[id].Definition.DependsOn {
			if _, included := closure[dependency]; !included {
				continue
			}
			indegree[id]++
			consumers[dependency] = append(consumers[dependency], id)
		}
	}
	ready := make([]string, 0)
	for id, count := range indegree {
		if count == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(closure))
	for len(ready) > 0 {
		current := ready[0]
		ready = ready[1:]
		order = append(order, current)
		sort.Strings(consumers[current])
		for _, consumer := range consumers[current] {
			indegree[consumer]--
			if indegree[consumer] == 0 {
				ready = insertSorted(ready, consumer)
			}
		}
	}
	if len(order) == len(closure) {
		return order, nil
	}
	return nil, fmt.Errorf("case dependency cycle: %s", stringsJoin(cyclePath(cases, closure), " -> "))
}

func cyclePath(cases map[string]testkit.Case, closure map[string]struct{}) []string {
	state := make(map[string]uint8, len(closure))
	stack := make([]string, 0, len(closure))
	var found []string
	var visit func(string) bool
	visit = func(id string) bool {
		state[id] = 1
		stack = append(stack, id)
		dependencies := append([]string(nil), cases[id].Definition.DependsOn...)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if _, included := closure[dependency]; !included {
				continue
			}
			if state[dependency] == 1 {
				start := 0
				for stack[start] != dependency {
					start++
				}
				found = append(append([]string(nil), stack[start:]...), dependency)
				return true
			}
			if state[dependency] == 0 && visit(dependency) {
				return true
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return false
	}
	ids := make([]string, 0, len(closure))
	for id := range closure {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if state[id] == 0 && visit(id) {
			return found
		}
	}
	return nil
}

func insertSorted(values []string, value string) []string {
	index := sort.SearchStrings(values, value)
	values = append(values, "")
	copy(values[index+1:], values[index:])
	values[index] = value
	return values
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func stringsJoin(values []string, separator string) string {
	result := ""
	for index, value := range values {
		if index > 0 {
			result += separator
		}
		result += value
	}
	return result
}
