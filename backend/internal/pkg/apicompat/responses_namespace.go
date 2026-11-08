package apicompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesNamespaceName identifies a function child in a Responses namespace.
// It aliases the chat bridge mapping so both native and bridged paths share one
// namespace identity contract.
type ResponsesNamespaceName = NamespacedToolName

// FlattenResponsesNamespaces converts Codex private namespace declarations into
// public Responses function tools and rewrites namespace-qualified request calls.
func FlattenResponsesNamespaces(req map[string]any) (map[string]ResponsesNamespaceName, bool, error) {
	return FlattenResponsesNamespacesExcept(req, nil)
}

// FlattenResponsesNamespacesExcept is FlattenResponsesNamespaces with a set of
// service-owned namespace names that must remain native in the request.
func FlattenResponsesNamespacesExcept(req map[string]any, preserved map[string]bool) (map[string]ResponsesNamespaceName, bool, error) {
	if req == nil {
		return nil, false, nil
	}
	// 工具声明可能位于顶层 req["tools"]，也可能位于 input[] 的 additional_tools 载体
	// （codex 0.166 官方形态：顶层 tools 为空，namespace/custom 子工具放在载体里）。
	// 两个来源都要进入摊平视野，撞名沿用既有检查。载体项本身保留在 req["input"]，
	// 不被删除（客户端声明保留 + 顶层补充摊平条目，属既有 FlattenResponsesNamespaces 语义）。
	declared := collectResponsesToolDeclarations(req)
	if len(declared) == 0 {
		return nil, false, nil
	}

	topLevel := make(map[string]bool)
	for _, raw := range declared {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.TrimSpace(stringValue(tool["type"]))
		name := strings.TrimSpace(stringValue(tool["name"]))
		if (typ == "function" || typ == "custom") && name != "" {
			topLevel[name] = true
		}
	}

	names := make(map[string]ResponsesNamespaceName)
	for _, raw := range declared {
		tool, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(stringValue(tool["type"])) != "namespace" {
			continue
		}
		namespace := strings.TrimSpace(stringValue(tool["name"]))
		if namespace == "" || preserved[namespace] {
			continue
		}
		for _, rawChild := range namespaceChildren(tool) {
			child, ok := rawChild.(map[string]any)
			if !ok || !isFlattenableNamespaceChild(stringValue(child["type"])) {
				continue
			}
			name := strings.TrimSpace(stringValue(child["name"]))
			if name == "" {
				continue
			}
			flat := FlattenNamespaceToolName(namespace, name)
			entry := ResponsesNamespaceName{Namespace: namespace, Name: name}
			if topLevel[flat] {
				return nil, false, fmt.Errorf("namespace tool %q/%q flattens to %q which conflicts with a top-level tool of the same name; this upstream cannot disambiguate them, rename one of the tools", namespace, name, flat)
			}
			if prev, exists := names[flat]; exists && prev != entry {
				return nil, false, fmt.Errorf("namespace tools %q/%q and %q/%q both flatten to %q; this upstream cannot disambiguate them, rename one of the tools", prev.Namespace, prev.Name, namespace, name, flat)
			}
			names[flat] = entry
		}
	}
	if len(names) == 0 {
		return nil, false, nil
	}

	flattened := make([]any, 0, len(declared)+len(names))
	seen := make(map[string]bool)
	for _, raw := range declared {
		tool, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(stringValue(tool["type"])) != "namespace" {
			flattened = append(flattened, raw)
			continue
		}
		namespace := strings.TrimSpace(stringValue(tool["name"]))
		if preserved[namespace] {
			flattened = append(flattened, raw)
			continue
		}
		for _, rawChild := range namespaceChildren(tool) {
			child, ok := rawChild.(map[string]any)
			if !ok || !isFlattenableNamespaceChild(stringValue(child["type"])) {
				continue
			}
			name := strings.TrimSpace(stringValue(child["name"]))
			flat := FlattenNamespaceToolName(namespace, name)
			if name == "" || seen[flat] {
				continue
			}
			seen[flat] = true
			flatChild := make(map[string]any, len(child))
			for key, value := range child {
				flatChild[key] = value
			}
			flatChild["name"] = flat
			flattened = append(flattened, flatChild)
		}
	}
	req["tools"] = flattened
	rewriteNamespaceQualifiedCalls(req["input"], names)
	if choice, ok := req["tool_choice"].(map[string]any); ok {
		choiceNamespace := strings.TrimSpace(stringValue(choice["name"]))
		if strings.TrimSpace(stringValue(choice["type"])) == "namespace" && !preserved[choiceNamespace] {
			req["tool_choice"] = "auto"
		} else {
			rewriteNamespaceQualifiedCall(choice, names)
		}
	}
	return names, true, nil
}

// RestoreResponsesNamespaceCalls restores flattened function calls in a JSON
// Responses payload to the namespace/name identity expected by Codex.
func RestoreResponsesNamespaceCalls(payload []byte, names map[string]ResponsesNamespaceName) ([]byte, bool, error) {
	if len(payload) == 0 || len(names) == 0 {
		return payload, false, nil
	}
	var value any
	if err := json.Unmarshal(payload, &value); err != nil {
		return payload, false, err
	}
	changed := restoreResponsesNamespaceValue(value, names)
	if !changed {
		return payload, false, nil
	}
	var rebuilt bytes.Buffer
	encoder := json.NewEncoder(&rebuilt)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return payload, false, err
	}
	return bytes.TrimSuffix(rebuilt.Bytes(), []byte("\n")), true, nil
}

// isFlattenableNamespaceChild reports whether a namespace child tool should be
// flattened into a top-level tool entry. Namespace children of type "function"
// or "custom" are flattened; any other type is left untouched.
func isFlattenableNamespaceChild(typ string) bool {
	switch strings.TrimSpace(typ) {
	case "function", "custom":
		return true
	default:
		return false
	}
}

// collectResponsesToolDeclarations gathers every tool declaration the flatten
// layer must inspect: the top-level tools field and the official
// additional_tools carrier items embedded in input[]. Newer Codex clients
// (0.166) declare their runtime namespace/custom tools in the carrier with an
// empty top-level tools field, so both sources must be scanned for namespace
// declarations. The carrier items themselves are left untouched in req["input"];
// only their flattened children are promoted into req["tools"] by the caller.
func collectResponsesToolDeclarations(req map[string]any) []any {
	var declared []any
	if raw, ok := req["tools"].([]any); ok {
		for _, item := range raw {
			if tool, ok := item.(map[string]any); ok {
				declared = append(declared, tool)
			}
		}
	}
	if input, ok := req["input"].([]any); ok {
		for _, item := range input {
			entry, ok := item.(map[string]any)
			if !ok || strings.TrimSpace(stringValue(entry["type"])) != "additional_tools" {
				continue
			}
			if subTools, ok := entry["tools"].([]any); ok {
				for _, child := range subTools {
					if tool, ok := child.(map[string]any); ok {
						declared = append(declared, tool)
					}
				}
			}
		}
	}
	return declared
}

func namespaceChildren(tool map[string]any) []any {
	if children, ok := tool["tools"].([]any); ok && len(children) > 0 {
		return children
	}
	children, _ := tool["children"].([]any)
	return children
}

func rewriteNamespaceQualifiedCalls(value any, names map[string]ResponsesNamespaceName) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			rewriteNamespaceQualifiedCalls(item, names)
		}
	case map[string]any:
		if strings.TrimSpace(stringValue(typed["type"])) == "function_call" {
			rewriteNamespaceQualifiedCall(typed, names)
		}
		for _, child := range typed {
			rewriteNamespaceQualifiedCalls(child, names)
		}
	}
}

func rewriteNamespaceQualifiedCall(item map[string]any, names map[string]ResponsesNamespaceName) bool {
	namespace := strings.TrimSpace(stringValue(item["namespace"]))
	name := strings.TrimSpace(stringValue(item["name"]))
	if namespace == "" || name == "" {
		return false
	}
	flat := FlattenNamespaceToolName(namespace, name)
	entry, ok := names[flat]
	if !ok || entry.Namespace != namespace || entry.Name != name {
		return false
	}
	item["name"] = flat
	delete(item, "namespace")
	return true
}

func restoreResponsesNamespaceValue(value any, names map[string]ResponsesNamespaceName) bool {
	changed := false
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			changed = restoreResponsesNamespaceValue(item, names) || changed
		}
	case map[string]any:
		if strings.TrimSpace(stringValue(typed["type"])) == "function_call" {
			if entry, ok := names[strings.TrimSpace(stringValue(typed["name"]))]; ok {
				typed["name"] = entry.Name
				typed["namespace"] = entry.Namespace
				changed = true
			}
		}
		for _, child := range typed {
			changed = restoreResponsesNamespaceValue(child, names) || changed
		}
	}
	return changed
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
