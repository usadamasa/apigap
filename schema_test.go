package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// yamlKeys は構造体の yaml タグから、その階層のキー一覧を集める。
func yamlKeys(t reflect.Type) map[string]reflect.Type {
	keys := map[string]reflect.Type{}
	for i := range t.NumField() {
		tag := t.Field(i).Tag.Get("yaml")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			keys[name] = t.Field(i).Type
		}
	}
	return keys
}

type schemaDoc struct {
	Properties map[string]schemaDoc `json:"properties"`
	Items      *schemaDoc           `json:"items"`
}

func loadSchema(t *testing.T, path string) schemaDoc {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s schemaDoc
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("%s の解析に失敗しました: %v", path, err)
	}
	return s
}

// assertKeysMatch は Go の構造体と JSON schema の properties が同じキーを持つことを確かめる。
// 設定項目を足したときに schema の更新を忘れると、エディタの補完と検証が嘘をつく。
func assertKeysMatch(t *testing.T, where string, typ reflect.Type, s schemaDoc) {
	t.Helper()
	keys := yamlKeys(typ)
	for k := range keys {
		if _, ok := s.Properties[k]; !ok {
			t.Errorf("%s: schema に %q がありません", where, k)
		}
	}
	for k := range s.Properties {
		if _, ok := keys[k]; !ok {
			t.Errorf("%s: schema の %q は Go 側に存在しません", where, k)
		}
	}
	for k, sub := range s.Properties {
		ft, ok := keys[k]
		if !ok {
			continue
		}
		switch {
		case ft.Kind() == reflect.Struct && ft != reflect.TypeOf(struct{}{}):
			assertKeysMatch(t, where+"."+k, ft, sub)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct && sub.Items != nil:
			assertKeysMatch(t, where+"."+k+"[]", ft.Elem(), *sub.Items)
		}
	}
}

func TestSchema_MatchesConfig(t *testing.T) {
	assertKeysMatch(t, "apigap.yaml", reflect.TypeOf(Config{}), loadSchema(t, "schema/apigap.schema.json"))
}

func TestSchema_MatchesScenario(t *testing.T) {
	assertKeysMatch(t, "scenario", reflect.TypeOf(Scenario{}), loadSchema(t, "schema/scenario.schema.json"))
}
