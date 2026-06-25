package adminroute

import "testing"

func TestValidateHost(t *testing.T) {
	valid := []string{"default", "play.example", "dev.lan"}
	for _, host := range valid {
		t.Run(host, func(t *testing.T) {
			if err := ValidateHost(host); err != nil {
				t.Fatalf("ValidateHost(%q) error = %v", host, err)
			}
		})
	}

	invalid := []string{"", " ", "play example", "play/example"}
	for _, host := range invalid {
		t.Run("invalid "+host, func(t *testing.T) {
			if err := ValidateHost(host); err == nil {
				t.Fatalf("ValidateHost(%q) error = nil, want error", host)
			}
		})
	}
}

func TestValidateUpstream(t *testing.T) {
	valid := []string{
		"127.0.0.1:25565",
		"localhost:25565",
		"kcp://127.0.0.1:25565",
		"quic://127.0.0.1:25565",
		"haproxy://127.0.0.1:25565",
	}
	for _, upstream := range valid {
		t.Run(upstream, func(t *testing.T) {
			if err := ValidateUpstream(upstream); err != nil {
				t.Fatalf("ValidateUpstream(%q) error = %v", upstream, err)
			}
		})
	}

	invalid := []string{"", "127.0.0.1", ":25565", "127.0.0.1:0", "127.0.0.1:70000", "127.0.0.1:not-a-port"}
	for _, upstream := range invalid {
		t.Run("invalid "+upstream, func(t *testing.T) {
			if err := ValidateUpstream(upstream); err == nil {
				t.Fatalf("ValidateUpstream(%q) error = nil, want error", upstream)
			}
		})
	}
}
