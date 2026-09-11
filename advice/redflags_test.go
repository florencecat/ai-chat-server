package advice

import (
	"os"
	"path/filepath"
	"testing"
)

func testFlags(t *testing.T, body string) *RedFlags {
	t.Helper()
	path := filepath.Join(t.TempDir(), "red-flags.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	rf, err := LoadRedFlags(path)
	if err != nil {
		t.Fatalf("LoadRedFlags: %v", err)
	}
	return rf
}

func TestLoadRedFlagsMissingFileIsNotAnError(t *testing.T) {
	rf, err := LoadRedFlags(filepath.Join(t.TempDir(), "нет.yaml"))
	if err != nil {
		t.Fatalf("LoadRedFlags: %v", err)
	}
	if rf.Len() != 0 {
		t.Errorf("Len = %d, want 0", rf.Len())
	}
}

func TestEscalateRaisesUrgency(t *testing.T) {
	rf := testFlags(t, "markers:\n  - судорог\n  - кровотечен\n")

	tests := []struct {
		name    string
		text    string
		urgency string
		want    string
		raised  bool
	}{
		{
			name:    "маркер при заниженном уровне",
			text:    "Похоже на судороги, но подождите до утра.",
			urgency: "self_care",
			want:    UrgencyVet24h,
			raised:  true,
		},
		{
			name:    "маркер без метаданных",
			text:    "Это кровотечение из носа.",
			urgency: "",
			want:    UrgencyVet24h,
			raised:  true,
		},
		{
			name:    "маркер при emergency — уровень не трогаем",
			text:    "Судороги, везите немедленно.",
			urgency: "emergency",
			want:    "emergency",
			raised:  false,
		},
		{
			name:    "маркер при vet_24h — уровень не трогаем",
			text:    "Судороги, к врачу сегодня.",
			urgency: UrgencyVet24h,
			want:    UrgencyVet24h,
			raised:  false,
		},
		{
			name:    "без маркера уровень не меняется и не понижается",
			text:    "Обычная линька, всё в порядке.",
			urgency: "self_care",
			want:    "self_care",
			raised:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &Answer{Response: tc.text, Urgency: tc.urgency}
			_, raised := rf.Escalate(a)
			if raised != tc.raised {
				t.Errorf("raised = %v, want %v", raised, tc.raised)
			}
			if a.Urgency != tc.want {
				t.Errorf("Urgency = %q, want %q", a.Urgency, tc.want)
			}
		})
	}
}

func TestMatchIsCaseInsensitive(t *testing.T) {
	rf := testFlags(t, "markers:\n  - Тепловой Удар\n")
	if hits := rf.Match("Возможен ТЕПЛОВОЙ УДАР."); len(hits) != 1 {
		t.Errorf("Match = %v, want one hit", hits)
	}
}

func TestNilRedFlagsIsSafe(t *testing.T) {
	var rf *RedFlags
	a := &Answer{Response: "судороги", Urgency: "self_care"}
	if _, raised := rf.Escalate(a); raised {
		t.Error("nil RedFlags must not escalate")
	}
	if rf.Len() != 0 {
		t.Error("nil RedFlags Len must be 0")
	}
}
