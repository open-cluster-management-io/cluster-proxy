package userserver

import (
	"strings"
	"sync"
	"testing"

	"open-cluster-management.io/cluster-proxy/pkg/utils"
)

// ---------------------------------------------------------------------------
// parseAllExposedServices
// ---------------------------------------------------------------------------

func TestParseAllExposedServices_SingleKey(t *testing.T) {
	data := map[string]string{
		"services": `
- namespace: monitoring
  service: prometheus
  port: "9090"
`,
	}
	services, err := parseAllExposedServices(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	if services[0].Namespace != "monitoring" || services[0].Service != "prometheus" {
		t.Errorf("unexpected service: %+v", services[0])
	}
}

func TestParseAllExposedServices_MultipleKeys(t *testing.T) {
	data := map[string]string{
		"team-a": `
- namespace: monitoring
  service: prometheus
`,
		"team-b": `
- namespace: default
  service: my-api
- namespace: default
  service: other-api
`,
	}
	services, err := parseAllExposedServices(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(services) != 3 {
		t.Fatalf("expected 3 services, got %d", len(services))
	}
}

func TestParseAllExposedServices_EmptyMap(t *testing.T) {
	services, err := parseAllExposedServices(map[string]string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(services) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(services))
	}
}

func TestParseAllExposedServices_EmptyKeyValue(t *testing.T) {
	data := map[string]string{
		"empty-key": "",
		"real-key": `
- namespace: monitoring
  service: prometheus
`,
	}
	services, err := parseAllExposedServices(data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(services) != 1 {
		t.Fatalf("expected 1 service (empty key skipped), got %d", len(services))
	}
}

func TestParseAllExposedServices_InvalidKey_ReturnsErrorWithKeyName(t *testing.T) {
	data := map[string]string{
		"bad-key": "this: is: not: valid: yaml: [[[",
	}
	_, err := parseAllExposedServices(data)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
	if !strings.Contains(err.Error(), "bad-key") {
		t.Errorf("expected error to contain key name %q, got: %v", "bad-key", err)
	}
}

func TestParseAllExposedServices_MissingNamespace_ReturnsErrorWithKeyName(t *testing.T) {
	data := map[string]string{
		"problem-key": `
- service: prometheus
`,
	}
	_, err := parseAllExposedServices(data)
	if err == nil {
		t.Fatal("expected error for missing namespace, got nil")
	}
	if !strings.Contains(err.Error(), "problem-key") {
		t.Errorf("expected error to contain key name %q, got: %v", "problem-key", err)
	}
}

// ---------------------------------------------------------------------------
// parseExposedServices
// ---------------------------------------------------------------------------

func TestParseExposedServices_Valid(t *testing.T) {
	yaml := `
- namespace: monitoring
  service: prometheus
  port: "9090"
  protocol: https
- namespace: default
  service: my-api
`
	services, err := parseExposedServices(yaml)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("expected 2 services, got %d", len(services))
	}

	if services[0].Namespace != "monitoring" || services[0].Service != "prometheus" ||
		services[0].Port != "9090" || services[0].Protocol != "https" {
		t.Errorf("services[0] mismatch: %+v", services[0])
	}
	if services[1].Namespace != "default" || services[1].Service != "my-api" ||
		services[1].Port != "" || services[1].Protocol != "" {
		t.Errorf("services[1] mismatch: %+v", services[1])
	}
}

func TestParseExposedServices_Empty(t *testing.T) {
	services, err := parseExposedServices("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(services) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(services))
	}
}

func TestParseExposedServices_MissingNamespace(t *testing.T) {
	yaml := `
- service: prometheus
  port: "9090"
`
	_, err := parseExposedServices(yaml)
	if err == nil {
		t.Fatal("expected error for missing namespace, got nil")
	}
}

func TestParseExposedServices_MissingService(t *testing.T) {
	yaml := `
- namespace: monitoring
  port: "9090"
`
	_, err := parseExposedServices(yaml)
	if err == nil {
		t.Fatal("expected error for missing service, got nil")
	}
}

func TestParseExposedServices_MalformedYAML(t *testing.T) {
	_, err := parseExposedServices("this: is: not: valid: yaml: [[[")
	if err == nil {
		t.Fatal("expected error for malformed YAML, got nil")
	}
}

// ---------------------------------------------------------------------------
// ExposedService.matches
// ---------------------------------------------------------------------------

func TestMatches_ExactMatch(t *testing.T) {
	e := ExposedService{Namespace: "monitoring", Service: "prometheus", Port: "9090", Protocol: "https"}
	tsc := utils.TargetServiceConfig{Namespace: "monitoring", Service: "prometheus", Port: "9090", Proto: "https"}
	if !e.matches(tsc) {
		t.Error("expected match")
	}
}

func TestMatches_WildcardPort(t *testing.T) {
	e := ExposedService{Namespace: "monitoring", Service: "prometheus"}
	tsc := utils.TargetServiceConfig{Namespace: "monitoring", Service: "prometheus", Port: "9090", Proto: "https"}
	if !e.matches(tsc) {
		t.Error("expected match when port is omitted")
	}
}

func TestMatches_WildcardProtocol(t *testing.T) {
	e := ExposedService{Namespace: "monitoring", Service: "prometheus", Port: "9090"}
	tsc := utils.TargetServiceConfig{Namespace: "monitoring", Service: "prometheus", Port: "9090", Proto: "http"}
	if !e.matches(tsc) {
		t.Error("expected match when protocol is omitted")
	}
}

func TestMatches_WrongNamespace(t *testing.T) {
	e := ExposedService{Namespace: "monitoring", Service: "prometheus"}
	tsc := utils.TargetServiceConfig{Namespace: "other", Service: "prometheus", Port: "9090", Proto: "https"}
	if e.matches(tsc) {
		t.Error("expected no match for wrong namespace")
	}
}

func TestMatches_WrongService(t *testing.T) {
	e := ExposedService{Namespace: "monitoring", Service: "prometheus"}
	tsc := utils.TargetServiceConfig{Namespace: "monitoring", Service: "grafana", Port: "3000", Proto: "https"}
	if e.matches(tsc) {
		t.Error("expected no match for wrong service")
	}
}

func TestMatches_WrongPort(t *testing.T) {
	e := ExposedService{Namespace: "monitoring", Service: "prometheus", Port: "9090"}
	tsc := utils.TargetServiceConfig{Namespace: "monitoring", Service: "prometheus", Port: "8080", Proto: "https"}
	if e.matches(tsc) {
		t.Error("expected no match for wrong port")
	}
}

func TestMatches_WrongProtocol(t *testing.T) {
	e := ExposedService{Namespace: "monitoring", Service: "prometheus", Protocol: "https"}
	tsc := utils.TargetServiceConfig{Namespace: "monitoring", Service: "prometheus", Port: "9090", Proto: "http"}
	if e.matches(tsc) {
		t.Error("expected no match for wrong protocol")
	}
}

// ---------------------------------------------------------------------------
// ServiceAllowlist.IsAllowed
// ---------------------------------------------------------------------------

func TestIsAllowed_EmptyList_DeniesAll(t *testing.T) {
	a := &ServiceAllowlist{}
	tsc := utils.TargetServiceConfig{Namespace: "monitoring", Service: "prometheus", Port: "9090", Proto: "https"}
	if a.IsAllowed(tsc) {
		t.Error("empty allowlist should deny all requests")
	}
}

func TestIsAllowed_MatchingEntry(t *testing.T) {
	a := &ServiceAllowlist{
		services: []ExposedService{
			{Namespace: "monitoring", Service: "prometheus", Port: "9090", Protocol: "https"},
		},
	}
	tsc := utils.TargetServiceConfig{Namespace: "monitoring", Service: "prometheus", Port: "9090", Proto: "https"}
	if !a.IsAllowed(tsc) {
		t.Error("expected request to be allowed")
	}
}

func TestIsAllowed_NoMatchingEntry(t *testing.T) {
	a := &ServiceAllowlist{
		services: []ExposedService{
			{Namespace: "monitoring", Service: "prometheus"},
		},
	}
	tsc := utils.TargetServiceConfig{Namespace: "default", Service: "other-service", Port: "8080", Proto: "http"}
	if a.IsAllowed(tsc) {
		t.Error("expected request to be denied")
	}
}

func TestIsAllowed_MultipleEntries_FirstMatches(t *testing.T) {
	a := &ServiceAllowlist{
		services: []ExposedService{
			{Namespace: "monitoring", Service: "prometheus"},
			{Namespace: "default", Service: "my-api"},
		},
	}
	tsc := utils.TargetServiceConfig{Namespace: "monitoring", Service: "prometheus", Port: "9090", Proto: "https"}
	if !a.IsAllowed(tsc) {
		t.Error("expected request to be allowed by first entry")
	}
}

func TestIsAllowed_MultipleEntries_SecondMatches(t *testing.T) {
	a := &ServiceAllowlist{
		services: []ExposedService{
			{Namespace: "monitoring", Service: "prometheus"},
			{Namespace: "default", Service: "my-api"},
		},
	}
	tsc := utils.TargetServiceConfig{Namespace: "default", Service: "my-api", Port: "8080", Proto: "http"}
	if !a.IsAllowed(tsc) {
		t.Error("expected request to be allowed by second entry")
	}
}

// ---------------------------------------------------------------------------
// ServiceAllowlist thread-safety
// ---------------------------------------------------------------------------

func TestServiceAllowlist_ConcurrentReadWrite(t *testing.T) {
	a := &ServiceAllowlist{}
	tsc := utils.TargetServiceConfig{Namespace: "ns", Service: "svc", Port: "80", Proto: "http"}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			a.IsAllowed(tsc)
		}()
		go func() {
			defer wg.Done()
			a.update([]ExposedService{{Namespace: "ns", Service: "svc"}})
		}()
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// ServiceAllowlist.update (keeps last-known-good on nil)
// ---------------------------------------------------------------------------

func TestServiceAllowlist_UpdateNilClearsList(t *testing.T) {
	a := &ServiceAllowlist{
		services: []ExposedService{{Namespace: "ns", Service: "svc"}},
	}
	a.update(nil)
	if a.Len() != 0 {
		t.Errorf("expected 0 entries after update(nil), got %d", a.Len())
	}
	tsc := utils.TargetServiceConfig{Namespace: "ns", Service: "svc"}
	if a.IsAllowed(tsc) {
		t.Error("expected deny-all after update(nil)")
	}
}
