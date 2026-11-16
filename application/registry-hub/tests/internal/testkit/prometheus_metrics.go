package testkit

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

func PrometheusSum(metrics []byte, name string, expectedLabels map[string]string) (float64, bool, error) {
	var total float64
	found := false
	scanner := bufio.NewScanner(strings.NewReader(string(metrics)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		field, rawValue, err := prometheusSampleLine(line)
		if err != nil {
			return 0, false, err
		}
		metricName, labels, err := prometheusSample(field)
		if err != nil {
			return 0, false, err
		}
		if metricName != name || !matchesLabels(labels, expectedLabels) {
			continue
		}
		value, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			return 0, false, fmt.Errorf("parse Prometheus sample %s: %w", name, err)
		}
		total += value
		found = true
	}
	if err := scanner.Err(); err != nil {
		return 0, false, err
	}
	return total, found, nil
}

func prometheusSampleLine(line string) (string, string, error) {
	start := strings.IndexByte(line, '{')
	end := -1
	if start < 0 {
		end = strings.IndexAny(line, " \t")
	} else {
		quoted, escaped := false, false
		for index := start + 1; index < len(line); index++ {
			if escaped {
				escaped = false
				continue
			}
			switch line[index] {
			case '\\':
				if quoted {
					escaped = true
				}
			case '"':
				quoted = !quoted
			case '}':
				if !quoted {
					end = index + 1
					index = len(line)
				}
			}
		}
	}
	if end <= 0 {
		return "", "", fmt.Errorf("malformed Prometheus sample")
	}
	field := line[:end]
	values := strings.Fields(line[end:])
	if len(values) == 0 {
		return "", "", fmt.Errorf("Prometheus sample has no value")
	}
	return field, values[0], nil
}

func prometheusSample(field string) (string, map[string]string, error) {
	start := strings.IndexByte(field, '{')
	if start < 0 {
		return field, nil, nil
	}
	if !strings.HasSuffix(field, "}") {
		return "", nil, fmt.Errorf("malformed Prometheus sample labels")
	}
	labels, err := parsePrometheusLabels(field[start+1 : len(field)-1])
	return field[:start], labels, err
}

func parsePrometheusLabels(raw string) (map[string]string, error) {
	labels := make(map[string]string)
	for raw = strings.TrimSpace(raw); raw != ""; raw = strings.TrimSpace(raw) {
		equal := strings.IndexByte(raw, '=')
		if equal < 1 || len(raw) <= equal+1 || raw[equal+1] != '"' {
			return nil, fmt.Errorf("malformed Prometheus label")
		}
		key := strings.TrimSpace(raw[:equal])
		end := quotedLabelEnd(raw[equal+1:])
		if end < 0 {
			return nil, fmt.Errorf("unterminated Prometheus label value")
		}
		quoted := raw[equal+1 : equal+1+end+1]
		value, err := strconv.Unquote(quoted)
		if err != nil {
			return nil, fmt.Errorf("decode Prometheus label %s: %w", key, err)
		}
		labels[key] = value
		raw = strings.TrimSpace(raw[equal+1+end+1:])
		if raw == "" {
			break
		}
		if raw[0] != ',' {
			return nil, fmt.Errorf("malformed Prometheus label separator")
		}
		raw = strings.TrimSpace(raw[1:])
	}
	return labels, nil
}

func quotedLabelEnd(raw string) int {
	escaped := false
	for index := 1; index < len(raw); index++ {
		if escaped {
			escaped = false
			continue
		}
		if raw[index] == '\\' {
			escaped = true
			continue
		}
		if raw[index] == '"' {
			return index
		}
	}
	return -1
}

func matchesLabels(actual, expected map[string]string) bool {
	for key, value := range expected {
		if actual[key] != value {
			return false
		}
	}
	return true
}
