package projectcontract

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type holder struct {
	Databases NameList `yaml:"databases"`
}

// TestNameListAcceptsRealContractShapes pins the three shapes observed in the
// operator's actual contracts. Before NameList these fields were declared
// []string, so the two map shapes failed the whole file and the product's
// deployment never reached the ledger at all.
func TestNameListAcceptsRealContractShapes(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"bare names": {
			body: "databases:\n  - synthetic_db\n  - app_healthtrack\n",
			want: []string{"synthetic_db", "app_healthtrack"},
		},
		"record with a database field": {
			body: "databases:\n  - authority: aws_rds\n    env_var: APP_DATABASE_URL\n    database: appdb\n    host: appdb.example.invalid\n",
			want: []string{"appdb"},
		},
		"single-key record": {
			body: "databases:\n  - aws_rds: product/control truth for users, entities, filings\n",
			want: []string{"aws_rds"},
		},
		"mixed shapes": {
			body: "databases:\n  - plain_db\n  - aws_rds: prose\n  - database: named\n",
			want: []string{"plain_db", "aws_rds", "named"},
		},
		"scalar instead of a list": {
			body: "databases: only_one\n",
			want: []string{"only_one"},
		},
		"absent": {
			body: "databases:\n",
			want: nil,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var got holder
			if err := yaml.Unmarshal([]byte(tc.body), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(got.Databases) != len(tc.want) {
				t.Fatalf("got %v, want %v", got.Databases, tc.want)
			}
			for i, want := range tc.want {
				if got.Databases[i] != want {
					t.Errorf("entry %d = %q, want %q", i, got.Databases[i], want)
				}
			}
		})
	}
}

// TestNameListRejectsUnnamedRecord keeps the tolerance from becoming silence. A
// record that identifies nothing must fail loudly: dropping it would make a
// product look like it depends on fewer databases than it does, which is a
// worse answer than a parse error the operator can see and fix.
func TestNameListRejectsUnnamedRecord(t *testing.T) {
	var got holder
	err := yaml.Unmarshal([]byte("databases:\n  - authority: aws_rds\n    env_var: SOME_URL\n"), &got)
	if err == nil {
		t.Fatalf("accepted a record that names no database: %v", got.Databases)
	}
	if !strings.Contains(err.Error(), "names nothing") {
		t.Errorf("error = %v, want it to say the record names nothing", err)
	}
}
