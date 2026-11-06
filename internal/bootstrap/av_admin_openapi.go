package bootstrap

import (
	v2 "av-platform/business/internal/businesses/v2"
	"av-platform/business/internal/platform/control"
	"reflect"
	"strings"
)

// Schema projection uses the same DTOs as the handlers, so generated clients
// cannot silently drift when an observability field changes.
func adminSchema(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		return adminSchema(t.Elem())
	}
	switch t.Kind() {
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer", "format": "int64"}
	case reflect.Slice:
		return map[string]any{"type": "array", "items": adminSchema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": true}
	case reflect.Struct:
		props := map[string]any{}
		required := []string{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")
			if tag[0] == "-" || tag[0] == "" {
				continue
			}
			props[tag[0]] = adminSchema(f.Type)
			if len(tag) == 1 {
				required = append(required, tag[0])
			}
		}
		return map[string]any{"type": "object", "properties": props, "required": required}
	default:
		return map[string]any{}
	}
}

func addAdminOpenAPI(paths, schemas map[string]any) {
	models := map[string]any{"AVAdminResource": v2.AdminResource{}, "AVAdminResourcePage": v2.AdminPage[v2.AdminResource]{}, "AVAdminOverview": v2.AdminOverview{}, "AVAdminDetail": v2.AdminDetail{}, "AVAdminOperation": v2.AdminOperation{}, "AVAdminOperationPage": v2.AdminPage[v2.AdminOperation]{}, "AVAdminAction": v2.AdminAction{}, "AVAdminActionResult": v2.AdminActionResult{}, "AVAdminNode": control.AdminNode{}}
	for name, value := range models {
		schemas[name] = adminSchema(reflect.TypeOf(value))
	}
	action := schemas["AVAdminAction"].(map[string]any)
	action["additionalProperties"] = false
	props := action["properties"].(map[string]any)
	props["action"] = map[string]any{"type": "string", "enum": []string{"end", "cancel", "revoke-control", "kick", "mute", "request-unmute", "chat-mute", "chat-unmute", "stage-remove"}}
	props["reason"] = map[string]any{"type": "string", "minLength": 1, "maxLength": 1000}
	props["version"] = map[string]any{"type": "integer", "format": "int64", "minimum": 0, "description": "Optional expected resource version; stale writes return 409"}
	for _, platform := range []bool{true, false} {
		prefix := "/api/v1/av/admin/platform/"
		description := "Platform administrator session required; API keys and resource credentials are rejected."
		if !platform {
			prefix = "/api/v1/av/admin/organizations/{orgId}/"
			description = "Platform administrator or current organization member with av.manage; does not require participation in the resource. API keys and resource credentials are rejected."
		}
		routes := map[string]string{"overview": "AVAdminOverview", "resources": "AVAdminResourcePage", "operations": "AVAdminOperationPage"}
		if platform {
			routes["media-nodes"] = "nodes"
		} else {
			routes["resources/{id}"] = "AVAdminDetail"
			routes["operations/{id}"] = "AVAdminOperation"
			routes["resources/{id}/actions"] = "AVAdminActionResult"
		}
		for suffix, model := range routes {
			method := "get"
			if strings.HasSuffix(suffix, "/actions") {
				method = "post"
			}
			path := prefix + suffix
			parameters := []any{}
			for _, part := range strings.Split(path, "/") {
				if strings.HasPrefix(part, "{") {
					parameters = append(parameters, map[string]any{"name": strings.Trim(part, "{}"), "in": "path", "required": true, "schema": map[string]any{"type": "string"}})
				}
			}
			if suffix == "resources" || suffix == "operations" {
				for _, name := range []string{"business", "state", "owner", "q", "from", "to", "resource_id"} {
					parameters = append(parameters, map[string]any{"name": name, "in": "query", "schema": map[string]any{"type": "string"}})
				}
				parameters = append(parameters, map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "default": 20, "minimum": 1, "maximum": 100}}, map[string]any{"name": "offset", "in": "query", "schema": map[string]any{"type": "integer", "default": 0, "minimum": 0, "maximum": 10000000}})
			}
			responseSchema := map[string]any{"$ref": "#/components/schemas/" + model}
			if model == "nodes" {
				responseSchema = map[string]any{"type": "object", "required": []string{"items"}, "properties": map[string]any{"items": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/AVAdminNode"}}}}
			}
			op := map[string]any{"operationId": "av_admin_" + method + "_" + strings.NewReplacer("/", "_", "{", "", "}", "", "-", "_").Replace(strings.TrimPrefix(path, "/api/v1/av/admin/")), "description": description, "security": []any{map[string]any{"cookie": []string{}}, map[string]any{"bearer": []string{}}}, "responses": map[string]any{"200": map[string]any{"description": "Management result. Accepted media changes finish asynchronously.", "content": map[string]any{"application/json": map[string]any{"schema": responseSchema}}}, "400": map[string]any{"description": "Invalid filter/action or missing idempotency key"}, "401": map[string]any{"description": "Login session required"}, "403": map[string]any{"description": "Management permission denied"}, "404": map[string]any{"description": "Resource not found in the selected organization"}, "409": map[string]any{"description": "Resource state, version or idempotency conflict"}}}
			if method == "post" {
				parameters = append(parameters, map[string]any{"name": "X-Foundation-Request", "in": "header", "required": true, "schema": map[string]any{"type": "string", "enum": []string{"1"}}}, map[string]any{"name": "Idempotency-Key", "in": "header", "required": true, "schema": map[string]any{"type": "string", "minLength": 1, "maxLength": 128}})
				op["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/AVAdminAction"}}}}
			}
			op["parameters"] = parameters
			paths[path] = map[string]any{method: op}
		}
	}
}
