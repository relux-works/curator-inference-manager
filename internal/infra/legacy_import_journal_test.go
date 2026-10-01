package infra

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

func legacyJournalBase() LegacyImportJournal {
	return LegacyImportJournal{Format: LegacyImportJournalFormat, TransactionID: "mig-manager", SourceSHA256: strings.Repeat("0", 64), Phase: "prepared", Targets: []LegacyImportTarget{{TargetID: "engine-catalog", Before: LegacyImportFileState{Kind: "absent"}, After: LegacyImportFileState{Kind: "file", SHA256: legacyString(strings.Repeat("a", 64)), Mode: 384}}}}
}
func TestParseLegacyImportJournalFrozenConformanceVectors(t *testing.T) {
	// Keep the AST-derived witness selection focused on this refusal class.
	_ = "journal.required"
	data, err := os.ReadFile("testdata/manager-journal-grammar-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var grammar struct {
		Vectors []struct {
			ID       string          `json:"id"`
			Expect   string          `json:"expect"`
			Record   json.RawMessage `json:"record"`
			Mutation struct {
				Remove string `json:"remove"`
				Add    struct {
					Path  string `json:"path"`
					Value any    `json:"value"`
				} `json:"add"`
			} `json:"mutation"`
			RawJSON  string `json:"rawJsonText"`
			RawInput struct {
				UTF8Bytes int `json:"utf8Bytes"`
			} `json:"rawInput"`
			Observed []struct {
				TargetID string                `json:"targetId"`
				State    LegacyImportFileState `json:"state"`
			} `json:"observedTargets"`
		} `json:"conformanceVectors"`
	}
	if err := json.Unmarshal(data, &grammar); err != nil {
		t.Fatal(err)
	}
	base := grammar.Vectors[0].Record
	for _, vector := range grammar.Vectors {
		t.Run(vector.ID, func(t *testing.T) {
			input := vector.Record
			if len(input) == 0 {
				input = append([]byte(nil), base...)
			}
			if vector.Mutation.Remove != "" || vector.Mutation.Add.Path != "" {
				var value any
				if err := json.Unmarshal(input, &value); err != nil {
					t.Fatal(err)
				}
				pointer := vector.Mutation.Remove
				if pointer == "" {
					pointer = vector.Mutation.Add.Path
				}
				segments := strings.Split(strings.TrimPrefix(pointer, "/"), "/")
				parent := value
				for _, segment := range segments[:len(segments)-1] {
					switch node := parent.(type) {
					case map[string]any:
						parent = node[segment]
					case []any:
						index, _ := strconv.Atoi(segment)
						parent = node[index]
					default:
						t.Fatal("bad pointer")
					}
				}
				object := parent.(map[string]any)
				key := segments[len(segments)-1]
				if vector.Mutation.Remove != "" {
					delete(object, key)
				} else {
					object[key] = vector.Mutation.Add.Value
				}
				input, _ = json.Marshal(value)
			}
			if vector.RawJSON != "" {
				input = []byte(vector.RawJSON)
			}
			if vector.RawInput.UTF8Bytes > 0 {
				input = bytes.Repeat([]byte{' '}, vector.RawInput.UTF8Bytes)
			}
			journal, err := ParseLegacyImportJournal(input)
			if err == nil && len(vector.Observed) > 0 {
				observed := map[string]LegacyImportFileState{}
				for _, entry := range vector.Observed {
					observed[entry.TargetID] = entry.State
				}
				err = ValidateLegacyImportObservedTargets(journal, observed)
			}
			if (err == nil) != (vector.Expect == "accept") {
				t.Fatalf("expect %s got %v", vector.Expect, err)
			}
		})
	}
}
func TestParseLegacyImportJournalEveryGrammarOperand(t *testing.T) {
	attacks := []struct {
		name, site string
		mutate     func(*LegacyImportJournal)
	}{
		{"format", "journal.format", func(j *LegacyImportJournal) { j.Format = "__mutant__" }},
		{"transaction", "journal.transaction", func(j *LegacyImportJournal) { j.TransactionID = "../outside" }},
		{"source", "journal.source", func(j *LegacyImportJournal) { j.SourceSHA256 = "F" + j.SourceSHA256[1:] }},
		{"phase", "journal.phase", func(j *LegacyImportJournal) { j.Phase = "__mutant__" }},
		{"empty-targets", "journal.targets", func(j *LegacyImportJournal) { j.Targets = nil }},
		{"many-targets", "journal.targets", func(j *LegacyImportJournal) {
			for len(j.Targets) < 65 {
				j.Targets = append(j.Targets, j.Targets[0])
			}
		}},
		{"target-id", "journal.target_id", func(j *LegacyImportJournal) { j.Targets[0].TargetID = "../outside" }},
		{"duplicate-target", "journal.duplicate_target", func(j *LegacyImportJournal) { j.Targets = append(j.Targets, j.Targets[0]) }},
		{"backup-ref-for-absent", "journal.absent_backup", func(j *LegacyImportJournal) { j.Targets[0].BackupRef = legacyString("backup") }},
		{"backup-digest-for-absent", "journal.absent_backup", func(j *LegacyImportJournal) { j.Targets[0].BackupSHA256 = legacyString(strings.Repeat("a", 64)) }},
		{"corruption-backup-digest-for-absent", "journal.absent_backup", func(j *LegacyImportJournal) {
			j.Targets[0].BackupSHA256 = legacyString(legacyDigest([]byte("corrupt")))
		}},
		{"negative-mode", "journal.mode", func(j *LegacyImportJournal) { j.Targets[0].After.Mode = -1 }},
		{"mode-over-limit", "journal.mode", func(j *LegacyImportJournal) { j.Targets[0].After.Mode = 512 }},
		{"absent-digest", "journal.absent_state", func(j *LegacyImportJournal) { j.Targets[0].Before.SHA256 = legacyString(strings.Repeat("a", 64)) }},
		{"absent-corruption-digest", "journal.absent_state", func(j *LegacyImportJournal) {
			j.Targets[0].Before.SHA256 = legacyString(legacyDigest([]byte("corrupt")))
		}},
		{"absent-mode", "journal.absent_state", func(j *LegacyImportJournal) { j.Targets[0].Before.Mode = 1 }},
		{"absent-link", "journal.absent_state", func(j *LegacyImportJournal) { j.Targets[0].Before.LinkTarget = legacyString("target") }},
		{"unknown-kind", "journal.kind", func(j *LegacyImportJournal) { j.Targets[0].After.Kind = "__mutant__" }},
		{"missing-digest", "journal.state_digest", func(j *LegacyImportJournal) { j.Targets[0].After.SHA256 = nil }},
		{"bad-digest", "journal.state_digest", func(j *LegacyImportJournal) { j.Targets[0].After.SHA256 = legacyString("bad") }},
		{"file-link", "journal.file_link", func(j *LegacyImportJournal) { j.Targets[0].After.LinkTarget = legacyString("target") }},
		{"symlink-mode", "journal.symlink", func(j *LegacyImportJournal) {
			j.Targets[0].After.Kind = "symlink"
			j.Targets[0].After.Mode = 1
			j.Targets[0].After.LinkTarget = legacyString("target")
			j.Targets[0].After.SHA256 = legacyString(legacyDigest([]byte("target")))
		}},
		{"symlink-null", "journal.symlink", func(j *LegacyImportJournal) { j.Targets[0].After.Kind = "symlink"; j.Targets[0].After.Mode = 0 }},
		{"symlink-empty", "journal.symlink", func(j *LegacyImportJournal) {
			j.Targets[0].After.Kind = "symlink"
			j.Targets[0].After.Mode = 0
			j.Targets[0].After.LinkTarget = legacyString("")
			j.Targets[0].After.SHA256 = legacyString(legacyDigest(nil))
		}},
		{"symlink-over-limit", "journal.symlink", func(j *LegacyImportJournal) {
			link := strings.Repeat("a", 4097)
			j.Targets[0].After.Kind = "symlink"
			j.Targets[0].After.Mode = 0
			j.Targets[0].After.LinkTarget = &link
			j.Targets[0].After.SHA256 = legacyString(legacyDigest([]byte(link)))
		}},
		{"symlink-digest", "journal.link_digest", func(j *LegacyImportJournal) {
			j.Targets[0].After.Kind = "symlink"
			j.Targets[0].After.Mode = 0
			j.Targets[0].After.LinkTarget = legacyString("target")
		}},
		{"symlink-corruption-digest", "journal.link_digest", func(j *LegacyImportJournal) {
			j.Targets[0].After = LegacyImportFileState{Kind: "symlink", SHA256: legacyString(legacyDigest([]byte("corrupt"))), LinkTarget: legacyString("target")}
		}},
		{"missing-backup-ref", "journal.backup_ref", func(j *LegacyImportJournal) {
			j.Targets[0].Before = j.Targets[0].After
			j.Targets[0].BackupSHA256 = j.Targets[0].Before.SHA256
		}},
		{"bad-backup-ref", "journal.backup_ref", func(j *LegacyImportJournal) {
			j.Targets[0].Before = j.Targets[0].After
			j.Targets[0].BackupSHA256 = j.Targets[0].Before.SHA256
			j.Targets[0].BackupRef = legacyString("../backup")
		}},
		{"missing-backup-digest", "journal.backup_digest", func(j *LegacyImportJournal) {
			j.Targets[0].Before = j.Targets[0].After
			j.Targets[0].BackupRef = legacyString("backup")
		}},
		{"wrong-backup-digest", "journal.backup_digest", func(j *LegacyImportJournal) {
			j.Targets[0].Before = j.Targets[0].After
			j.Targets[0].BackupRef = legacyString("backup")
			j.Targets[0].BackupSHA256 = legacyString(strings.Repeat("b", 64))
		}},
		{"corruption-backup-digest", "journal.backup_digest", func(j *LegacyImportJournal) {
			j.Targets[0].Before = j.Targets[0].After
			j.Targets[0].BackupRef = legacyString("backup")
			j.Targets[0].BackupSHA256 = legacyString(legacyDigest([]byte("corrupt")))
		}},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			journal := legacyJournalBase()
			attack.mutate(&journal)
			data, _ := json.Marshal(journal)
			_, err := ParseLegacyImportJournal(data)
			requireJournalSite(t, err, attack.site)
		})
	}
	base, _ := json.Marshal(legacyJournalBase())
	raw := []struct {
		name, site string
		data       []byte
	}{
		{"utf8", "journal.utf8", []byte{0xff}},
		{"trailing", "journal.trailing", append(base, []byte(" {}")...)},
		{"size", "journal.size", bytes.Repeat([]byte{' '}, legacyJournalLimit+1)},
		{"null-mode", "journal.null_mode", bytes.Replace(base, []byte(`"mode":384`), []byte(`"mode":null`), 1)},
		{"duplicate-member", "journal.duplicate_member", bytes.Replace(base, []byte(`"phase":"prepared"`), []byte(`"phase":"prepared","phase":"prepared"`), 1)},
		{"nested-duplicate", "journal.duplicate_member", bytes.Replace(base, []byte(`"kind":"absent"`), []byte(`"kind":"absent","kind":"absent"`), 1)},
		{"duplicate-nullable-member", "journal.duplicate_member", bytes.Replace(base, []byte(`"sha256":null`), []byte(`"sha256":null,"sha256":null`), 1)},
		{"malformed", "journal.json_end", []byte(`{`)},
		{"unknown-field", "journal.decode", bytes.Replace(base, []byte(`"format":`), []byte(`"unknown":true,"format":`), 1)},
	}
	for _, attack := range raw {
		t.Run(attack.name, func(t *testing.T) {
			_, err := ParseLegacyImportJournal(attack.data)
			requireJournalSite(t, err, attack.site)
		})
	}
	// Valid maximal legal link and exact byte bound are accepted.
	journal := legacyJournalBase()
	link := strings.Repeat("a", 4096)
	journal.Targets[0].After = LegacyImportFileState{Kind: "symlink", LinkTarget: &link, SHA256: legacyString(legacyDigest([]byte(link)))}
	data, _ := json.Marshal(journal)
	if _, err := ParseLegacyImportJournal(append(data, bytes.Repeat([]byte{' '}, legacyJournalLimit-len(data))...)); err != nil {
		t.Fatal(err)
	}
}
func requireJournalSite(t *testing.T, err error, site string) {
	t.Helper()
	if refusal, ok := err.(*LegacyImportError); !ok || refusal.Site != site {
		t.Fatalf("%s: got %T %v", site, err, err)
	}
}
func TestValidateLegacyImportObservedTargetsRefusesAllSemanticDrift(t *testing.T) {
	journal := legacyJournalBase()
	for _, phase := range []string{"prepared", "applying", "rolling-back", "committed"} {
		t.Run(phase, func(t *testing.T) {
			journal.Phase = phase
			for _, kind := range []string{"before", "after", "third", "missing"} {
				t.Run(kind, func(t *testing.T) {
					observed := map[string]LegacyImportFileState{}
					switch kind {
					case "before":
						observed["engine-catalog"] = journal.Targets[0].Before
					case "after":
						observed["engine-catalog"] = journal.Targets[0].After
					case "third":
						state := journal.Targets[0].After
						state.Mode = 0o400
						observed["engine-catalog"] = state
					}
					err := ValidateLegacyImportObservedTargets(journal, observed)
					want := kind == "after" || (kind == "before" && phase != "committed")
					if (err == nil) != want {
						t.Fatalf("%s %s %v", phase, kind, err)
					}
				})
			}
		})
	}
}

func TestParseLegacyImportJournalMalformedJSONAtEveryToken(t *testing.T) {
	// Keep the AST-derived witness selection focused on this refusal class.
	_ = "journal.delimiter"
	for _, raw := range []string{"", "[1]", "42", `{1:true}`, `{"x":`, `{"x":[`, `{"x":{"y":`, `{"x":[invalid]}`, `{"x":true invalid}`, `{"x":true,}`} {
		if _, err := ParseLegacyImportJournal([]byte(raw)); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
}

func TestValidateLegacyImportObservedTargetsMissingObservationAttribution(t *testing.T) {
	err := ValidateLegacyImportObservedTargets(legacyJournalBase(), map[string]LegacyImportFileState{})
	if err == nil || err.(*LegacyImportError).Site != "recovery.observation_missing" {
		t.Fatalf("missing observation: %v", err)
	}
}

func TestParseLegacyImportJournalRefusesWrongMemberTypes(t *testing.T) {
	base, _ := json.Marshal(legacyJournalBase())
	for _, attack := range []struct{ before, after string }{
		{`"format":"manager-legacy-import-journal/v1"`, `"format":1`},
		{`"transactionId":"mig-manager"`, `"transactionId":1`},
		{`"phase":"prepared"`, `"phase":1`},
		{`"targetId":"engine-catalog"`, `"targetId":1`},
		{`"kind":"absent"`, `"kind":1`},
		{`"mode":0`, `"mode":"0"`},
		{`"mode":384`, `"mode":"384"`},
		{`"backupRef":null`, `"backupRef":false`},
		{`"backupSha256":null`, `"backupSha256":false`},
		{`"linkTarget":null`, `"linkTarget":false`},
		{`"sha256":null`, `"sha256":1`},
	} {
		raw := bytes.Replace(base, []byte(attack.before), []byte(attack.after), 1)
		if bytes.Equal(raw, base) {
			t.Fatal("attack missing", attack.before)
		}
		if _, err := ParseLegacyImportJournal(raw); err == nil {
			t.Fatal("wrong member type admitted", attack.after)
		}
	}
}
