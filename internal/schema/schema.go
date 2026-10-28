// Package schema exports machine-readable contract schemas and validates
// concrete documents through the same package validators Compound uses at run
// time. JSON Schema documents are structural; Validate is the semantic gate.
package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"sort"
	"strings"

	"compound/internal/capgraph"
	"compound/internal/decisions"
	"compound/internal/distiller"
	"compound/internal/envelope"
	"compound/internal/knowledge"
	"compound/internal/panel"
	"compound/internal/product"
	"compound/internal/routeevidence"
	"compound/internal/runtimebinding"
	"gopkg.in/yaml.v3"
)

type Contract struct {
	Name        string
	Description string
	Type        reflect.Type
	Validate    func(path string, data []byte) error
}

func Contracts() []Contract {
	contracts := []Contract{
		{Name: "trace_envelope", Description: "runtime trace envelope", Type: reflect.TypeOf(envelope.TraceEnvelope{}), Validate: validateEnvelope[envelope.TraceEnvelope]},
		{Name: "context_pack", Description: "agent context pack", Type: reflect.TypeOf(envelope.ContextPack{}), Validate: validateEnvelope[envelope.ContextPack]},
		{Name: "work_envelope", Description: "unit-of-work envelope", Type: reflect.TypeOf(envelope.WorkEnvelope{}), Validate: validateEnvelope[envelope.WorkEnvelope]},
		{Name: "action_intent", Description: "pre-policy action intent", Type: reflect.TypeOf(envelope.ActionIntent{}), Validate: validateEnvelope[envelope.ActionIntent]},
		{Name: "policy_decision", Description: "policy verdict", Type: reflect.TypeOf(envelope.PolicyDecision{}), Validate: validateEnvelope[envelope.PolicyDecision]},
		{Name: "receipt", Description: "work receipt", Type: reflect.TypeOf(envelope.Receipt{}), Validate: validateEnvelope[envelope.Receipt]},
		{Name: "transition", Description: "typed transition proof", Type: reflect.TypeOf(envelope.Transition{}), Validate: validateEnvelope[envelope.Transition]},
		{Name: "candidate_set", Description: "distiller candidate set", Type: reflect.TypeOf(distiller.Trace{}), Validate: validateCandidateSet},

		{Name: "knowledge_export", Description: "generic knowledge source export", Type: reflect.TypeOf(knowledge.Export{}), Validate: validateKnowledgeExport},
		{Name: "knowledge_extraction_request", Description: "knowledge extraction work item", Type: reflect.TypeOf(knowledge.ExtractionRequest{}), Validate: validateKnowledgeExtractionRequest},
		{Name: "runtime_binding", Description: "RuntimeBinding and ServiceContract bundle", Type: reflect.TypeOf(runtimebinding.Bundle{}), Validate: validateRuntimeBinding},
		{Name: "decision_lifecycle", Description: "decision lifecycle object", Type: reflect.TypeOf(decisions.Lifecycle{}), Validate: validateDecisionLifecycle},
		{Name: "capability_node", Description: "CapabilityGraph node", Type: reflect.TypeOf(capgraph.Node{}), Validate: validateCapabilityNode},
		{Name: "product_signal", Description: "product signal document", Type: reflect.TypeOf(product.SignalDocument{}), Validate: validateProductSignal},
		{Name: "panel_run", Description: "perspective panel run record", Type: reflect.TypeOf(panel.Run{}), Validate: validatePanelRun},

		{Name: "product_signal_export", Description: "product signal source-export document", Type: reflect.TypeOf(product.SignalExportDocument{}), Validate: validateProductSignalExport},

		{Name: "product_loops", Description: "product feedback loop read model", Type: reflect.TypeOf(product.LoopModel{}), Validate: validateProductLoops},
		{Name: "route_evidence", Description: "execution route evidence", Type: reflect.TypeOf(routeevidence.Evidence{}), Validate: validateRouteEvidence},
	}
	sort.Slice(contracts, func(i, j int) bool { return contracts[i].Name < contracts[j].Name })
	return contracts
}

func Get(name string) (Contract, error) {
	want := normalizeName(name)
	for _, contract := range Contracts() {
		if contract.Name == want {
			return contract, nil
		}
	}
	return Contract{}, fmt.Errorf("unknown schema %q", name)
}

func Export(name string) (map[string]any, error) {
	contract, err := Get(name)
	if err != nil {
		return nil, err
	}
	doc := schemaFor(contract.Type)
	doc["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	doc["$id"] = "compound://schemas/" + contract.Name
	doc["title"] = contract.Name
	doc["description"] = contract.Description
	return doc, nil
}

func Validate(name, path string) error {
	contract, err := Get(name)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := contract.Validate(path, data); err != nil {
		return fmt.Errorf("%s invalid: %w", contract.Name, err)
	}
	return nil
}

func normalizeName(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	name = strings.ReplaceAll(name, "-", "_")
	return name
}

func schemaFor(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		return schemaForStruct(t)
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Interface:
		return map[string]any{}
	default:
		return map[string]any{"type": "object"}
	}
}

func schemaForStruct(t reflect.Type) map[string]any {
	props := map[string]any{}
	var required []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		name, omitEmpty, skip := jsonField(field)
		if skip {
			continue
		}
		if field.Anonymous && name == "" {
			nested := schemaFor(field.Type)
			if nestedProps, ok := nested["properties"].(map[string]any); ok {
				for k, v := range nestedProps {
					props[k] = v
				}
			}
			if nestedReq, ok := nested["required"].([]string); ok {
				required = append(required, nestedReq...)
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(field.Name[:1]) + field.Name[1:]
		}
		props[name] = schemaFor(field.Type)
		if !omitEmpty {
			required = append(required, name)
		}
	}
	if t == reflect.TypeOf(envelope.StateDelta{}) {
		knownRequired := append([]string(nil), required...)
		unknownRequired := []string{"change_knowledge"}
		for _, field := range required {
			if field != "changed" {
				unknownRequired = append(unknownRequired, field)
			}
		}
		props["change_knowledge"] = map[string]any{"const": "unknown"}
		return map[string]any{"type": "object", "properties": props, "additionalProperties": false, "oneOf": []any{
			map[string]any{"required": knownRequired, "not": map[string]any{"required": []string{"change_knowledge"}}},
			map[string]any{"required": unknownRequired, "not": map[string]any{"required": []string{"changed"}}},
		}}
	}
	sort.Strings(required)
	result := map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
	if t == reflect.TypeOf(envelope.Receipt{}) {
		result["allOf"] = []any{map[string]any{
			"if":   map[string]any{"properties": map[string]any{"outcome": map[string]any{"enum": []string{"completed", "no_op"}}}, "required": []string{"outcome"}},
			"then": map[string]any{"properties": map[string]any{"state_delta": map[string]any{"not": map[string]any{"required": []string{"change_knowledge"}}}}},
		}}
	}
	return result
}

func jsonField(field reflect.StructField) (name string, omitEmpty bool, skip bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	for _, part := range parts[1:] {
		if part == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty, false
}

func validateEnvelope[T any](path string, data []byte) error {
	var doc T
	if err := decodeStrictJSON(path, data, &doc); err != nil {
		return err
	}
	env, ok := any(&doc).(envelope.Envelope)
	if !ok {
		return fmt.Errorf("%T does not implement envelope.Envelope", doc)
	}
	return env.Validate()
}

func validateCandidateSet(_ string, data []byte) error {
	_, err := (distiller.ReferenceExtractor{}).Extract(data, distiller.DefaultPolicy())
	return err
}

func validateRuntimeBinding(path string, _ []byte) error {
	bundle, err := runtimebinding.Load(path)
	if err != nil {
		return err
	}
	report := runtimebinding.Doctor(bundle)
	if report.Status != "ok" {
		return fmt.Errorf("runtime doctor status=%s", report.Status)
	}
	return nil
}

func validateDecisionLifecycle(path string, data []byte) error {
	var doc decisions.Lifecycle
	if err := decodeJSONOrYAML(path, data, &doc); err != nil {
		return err
	}
	var missing []string
	if strings.TrimSpace(doc.ID) == "" {
		missing = append(missing, "id")
	}
	if strings.TrimSpace(doc.Kind) == "" {
		missing = append(missing, "kind")
	}
	if strings.TrimSpace(doc.Choice) == "" {
		missing = append(missing, "choice")
	}
	if strings.TrimSpace(doc.Status) == "" {
		missing = append(missing, "status")
	}
	if decisions.CanonicalStatus(doc.Status) == decisions.StatusAccepted {
		missing = append(missing, decisions.MissingRequired(doc)...)
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}
	return nil
}

func validateCapabilityNode(path string, data []byte) error {
	var node capgraph.Node
	if err := decodeJSONOrYAML(path, data, &node); err != nil {
		return err
	}
	if strings.TrimSpace(node.ID) == "" {
		return fmt.Errorf("id required")
	}
	if !contains(capgraph.NodeKinds, node.Kind) {
		return fmt.Errorf("kind must be one of %v, got %q", capgraph.NodeKinds, node.Kind)
	}
	if strings.TrimSpace(node.Name) == "" {
		return fmt.Errorf("name required")
	}
	return nil
}

func validateProductSignal(path string, _ []byte) error {
	report := product.SignalDoctor(path)
	if report.Status != "ok" {
		return fmt.Errorf("product signal doctor status=%s", report.Status)
	}
	return nil
}

func validatePanelRun(_ string, data []byte) error {
	run, err := panel.Decode(data)
	if err != nil {
		return err
	}
	return run.Validate()
}

func validateKnowledgeExport(_ string, data []byte) error {
	_, err := knowledge.DecodeExport(data)
	return err
}

func validateKnowledgeExtractionRequest(path string, data []byte) error {
	var req knowledge.ExtractionRequest
	if err := decodeStrictJSON(path, data, &req); err != nil {
		return err
	}
	return req.Validate()
}

func validateProductSignalExport(path string, data []byte) error {
	var doc product.SignalExportDocument
	if err := decodeJSONOrYAML(path, data, &doc); err != nil {
		return err
	}
	return product.ValidateSignalExportDocument(doc)
}

func validateProductLoops(path string, data []byte) error {
	var model product.LoopModel
	if err := decodeStrictJSON(path, data, &model); err != nil {
		return err
	}
	return model.Validate()
}

func validateRouteEvidence(_ string, data []byte) error {
	_, err := routeevidence.Decode(data)
	return err
}

func decodeStrictJSON(path string, data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode %s: trailing JSON", path)
	}
	return nil
}

func decodeJSONOrYAML(path string, data []byte, dst any) error {
	if strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml") {
		return decodeYAML(data, dst)
	}
	return decodeStrictJSON(path, data, dst)
}

func decodeYAML(data []byte, dst any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(dst)
}

func contains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
