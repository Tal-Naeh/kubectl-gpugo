package main

import "testing"

func TestParseExporterSpecs(t *testing.T) {
	got, err := parseExporterSpecs([]string{"gpu-operator/nvidia-dcgm-exporter-abc:9400", "monitoring/enricher-x:9101"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Namespace != "gpu-operator" || got[0].Name != "nvidia-dcgm-exporter-abc" || got[0].Port != 9400 {
		t.Errorf("parsed: %+v", got)
	}
	for _, bad := range []string{"no-slash:9400", "ns/pod", "ns/pod:notaport", "ns/pod:0"} {
		if _, err := parseExporterSpecs([]string{bad}); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}
