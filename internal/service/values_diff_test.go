package service

import (
	"reflect"
	"testing"
)

func TestChangedLines(t *testing.T) {
	cases := []struct {
		name     string
		values   string
		defaults string
		want     []int
	}{
		{
			name: "computed and instance values over the defaults",
			// 1-based lines of the values ConfigMap text
			values: `extraEnv:
  EXTRA: "yes"
global:
  okdp:
    ingress:
      suffix: okdp.sandbox
image:
  repository: mirror.example.org/mini
  tag: "1.0"
ports:
- 8080
- 9090
rbac: false
replicaCount: 1
`,
			defaults: `# upstream defaults, with comments and another key order
replicaCount: 1
image:
  repository: example.org/mini   # replaced
  tag: "1.0"
ports: [8080]
rbac: true
extraEnv: {}
`,
			// EXTRA (new key), the whole global block (absent from the
			// defaults), image.repository, the appended port, rbac.
			want: []int{2, 3, 4, 5, 6, 8, 12, 13},
		},
		{
			name:     "identical values mark nothing",
			values:   "a: 1\nb:\n  c: [p, q]\n",
			defaults: "b: {c: [p, q]}\na: 1.0\n",
			want:     []int{},
		},
		{
			name: "a multi-line string marks all its lines",
			values: `config: |
  line one
  line two
other: x
`,
			defaults: "config: line one\nother: x\n",
			want:     []int{1, 2, 3},
		},
		{
			name: "YAML 1.1 booleans of the defaults read as Helm reads them",
			values: `enabled: true
mode: "yes"
`,
			defaults: "enabled: yes\nmode: \"yes\"\n",
			want:     []int{},
		},
		{
			name:     "a null over a default value",
			values:   "a: null\nb: 2\n",
			defaults: "a: {x: 1}\nb: 2\n",
			want:     []int{1},
		},
		{
			name: "a list item changed inside a list of maps",
			values: `env:
- name: A
  value: "1"
- name: B
  value: changed
`,
			defaults: "env: [{name: A, value: \"1\"}, {name: B, value: \"2\"}]\n",
			want:     []int{5},
		},
		{
			name:     "an emptied list marks its own line",
			values:   "a: []\nb: 1\n",
			defaults: "a: [1, 2]\nb: 1\n",
			want:     []int{1},
		},
		{
			name:     "no defaults: everything is computed",
			values:   "a: 1\nb:\n  c: 2\n",
			defaults: "",
			want:     []int{1, 2, 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ChangedLines(tc.values, []byte(tc.defaults))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ChangedLines = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChangedLinesRefusesUnparsableDefaults(t *testing.T) {
	if _, err := ChangedLines("a: 1\n", []byte("a: [")); err == nil {
		t.Fatal("want an error for defaults that do not parse")
	}
}
