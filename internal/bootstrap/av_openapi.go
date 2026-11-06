package bootstrap

import (
	"encoding/json"
	"fmt"
	"strings"

	v2 "av-platform/business/internal/businesses/v2"
	"platform-foundation/server/contracts"
)

// AVOpenAPI composes the real Foundation contract and AV routes, rejecting collisions.
func AVOpenAPI() ([]byte, error) {
	var document map[string]any
	if e := json.Unmarshal(contracts.OpenAPI, &document); e != nil {
		return nil, e
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid Foundation contract")
	}
	fullPaths := map[string]any{}
	for path, value := range paths {
		fullPaths["/api/v1"+path] = value
	}
	paths = fullPaths
	document["paths"] = paths
	document["servers"] = []any{map[string]string{"url": "/"}}
	document["info"] = map[string]any{"title": "AV Platform business API", "version": "1.0.0"}
	components := document["components"].(map[string]any)
	schemes := components["securitySchemes"].(map[string]any)
	schemes["avResource"] = map[string]any{"type": "apiKey", "in": "header", "name": "X-AV-Resource-Credential"}
	schemas := components["schemas"].(map[string]any)
	schemas["AVMediaOperation"] = map[string]any{
		"type": "object", "required": []string{"id", "resource_id", "state", "action", "updated"},
		"properties": map[string]any{
			"id": map[string]string{"type": "string"}, "resource_id": map[string]string{"type": "string"},
			"state":  map[string]any{"type": "string", "enum": []string{"queued", "retrying", "succeeded", "superseded", "failed"}},
			"action": map[string]string{"type": "string"}, "updated": map[string]string{"type": "integer", "format": "int64", "description": "Unix milliseconds"},
		},
	}
	routes := []v2.Route{}
	actions := map[string][]string{
		"screen-cast/sessions/{id}/{action}":    {"heartbeat", "connect", "stop"},
		"remote-control/sessions/{id}/{action}": {"heartbeat", "request", "connect", "stop"},
		"control-grants/{id}/{action}":          {"approve", "reject", "renew", "revoke"},
		"conference/meetings/{id}/{action}":     {"start", "edit", "connect", "credentials", "end", "cancel", "admit", "reject", "lock", "transfer", "kick", "mute", "mute-all", "request-unmute", "share-start", "share-stop", "leave", "presence"},
		"live-studio/broadcasts/{id}/{action}":  {"heartbeat", "connect", "end", "chat", "gift", "stage-request", "stage-approve", "stage-reject", "stage-leave", "stage-remove", "mute", "kick", "chat-delete", "pk-request", "pk-accept", "pk-reject", "pk-cancel", "pk-end", "leave"},
	}
	for _, route := range v2.Routes() {
		if values, ok := actions[route.Path]; ok {
			for _, action := range values {
				copy := route
				copy.Path = strings.ReplaceAll(route.Path, "{action}", action)
				copy.Sensitive = v2.Sensitive(copy.Method, copy.Path)
				routes = append(routes, copy)
			}
		} else {
			routes = append(routes, route)
		}
	}
	routes = append(routes, v2.Route{Method: "POST", Path: "rtc-grants/{id}/credentials", Sensitive: true}, v2.Route{Method: "GET", Path: "operations/{id}"}, v2.Route{Method: "POST", Path: "realtime/tickets", Sensitive: true}, v2.Route{Method: "GET", Path: "realtime/ws", Public: true}, v2.Route{Method: "GET", Path: "events"}, v2.Route{Method: "GET", Path: "capabilities", Public: true}, v2.Route{Method: "POST", Path: "organizations/initialize"})
	routes = append(routes, v2.Route{Method: "GET", Path: "operations"})
	for _, r := range routes {
		path := "/api/v1/av/" + r.Path
		item, exists := paths[path]
		if !exists {
			item = map[string]any{}
			paths[path] = item
		}
		methods := item.(map[string]any)
		method := strings.ToLower(r.Method)
		if _, exists = methods[method]; exists {
			return nil, fmt.Errorf("OpenAPI route conflict: %s %s", r.Method, path)
		}
		operation := map[string]any{"operationId": "av_" + method + "_" + strings.NewReplacer("/", "_", "{", "", "}", "", "-", "_").Replace(r.Path), "responses": map[string]any{"200": map[string]any{"description": "Business result; media operations complete asynchronously"}, "400": map[string]any{"description": "Invalid request or unsupported idempotency"}, "401": map[string]any{"description": "Authentication required"}, "403": map[string]any{"description": "Scope or resource authorization denied"}, "409": map[string]any{"description": "State conflict or media_preparing"}, "503": map[string]any{"description": "Dependency or media capacity unavailable"}}, "x-sensitive-response": r.Sensitive}
		parameters := []any{}
		for _, part := range strings.Split(r.Path, "/") {
			if strings.HasPrefix(part, "{") {
				parameters = append(parameters, map[string]any{"in": "path", "name": strings.Trim(part, "{}"), "required": true, "schema": map[string]string{"type": "string"}})
			}
		}
		parameters = append(parameters, map[string]any{"in": "header", "name": "X-Organization-ID", "schema": map[string]string{"type": "string"}}, map[string]any{"in": "header", "name": "X-AV-Resource-Credential", "schema": map[string]string{"type": "string"}})
		if r.Public {
			operation["security"] = []any{}
		} else {
			security := []any{map[string]any{"cookie": []string{}}, map[string]any{"bearer": []string{}}}
			if r.Path != "organizations/initialize" {
				security = append(security, map[string]any{"avResource": []string{}})
			}
			operation["security"] = security
		}
		if r.Method == "POST" {
			parameters = append(parameters, map[string]any{"in": "header", "name": "X-Foundation-Request", "required": true, "schema": map[string]any{"type": "string", "enum": []string{"1"}}})
			if !r.Sensitive {
				parameters = append(parameters, map[string]any{"in": "header", "name": "Idempotency-Key", "schema": map[string]any{"type": "string", "maxLength": 128}})
			}
		}
		if r.Path == "events" || r.Path == "operations" {
			parameters = append(parameters, map[string]any{"in": "query", "name": "resource_id", "required": true, "schema": map[string]string{"type": "string"}})
		}
		if r.Path == "events" {
			parameters = append(parameters, map[string]any{"in": "query", "name": "after", "schema": map[string]any{"type": "integer", "minimum": 0}})
		}
		if r.Path == "realtime/ws" {
			parameters = append(parameters, map[string]any{"in": "query", "name": "ticket", "required": true, "schema": map[string]string{"type": "string"}}, map[string]any{"in": "query", "name": "after", "schema": map[string]any{"type": "integer", "minimum": 0}})
		}
		responses := operation["responses"].(map[string]any)
		responses["200"].(map[string]any)["content"] = map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object", "additionalProperties": true}}}
		if r.Path == "operations" || r.Path == "operations/{id}" {
			var schema any = map[string]string{"$ref": "#/components/schemas/AVMediaOperation"}
			if r.Path == "operations" {
				schema = map[string]any{"type": "object", "required": []string{"items"}, "properties": map[string]any{"items": map[string]any{"type": "array", "maxItems": 200, "items": schema}}}
			}
			responses["200"].(map[string]any)["content"] = map[string]any{"application/json": map[string]any{"schema": schema}}
		}
		if r.Path == "realtime/ws" {
			responses["101"] = map[string]any{"description": "WebSocket sends individual resource events; reconnect using a fresh ticket at most every 60 seconds"}
		}
		operation["parameters"] = parameters
		if r.Method == "POST" {
			properties := map[string]any{}
			for _, field := range []string{"title", "name", "code", "password", "number", "starts_at", "target", "content", "resource_id"} {
				properties[field] = map[string]string{"type": "string"}
			}
			for _, field := range []string{"scheduled", "waiting_room", "enabled", "local_consent", "stage"} {
				properties[field] = map[string]string{"type": "boolean"}
			}
			for _, field := range []string{"duration_minutes", "amount", "epoch"} {
				properties[field] = map[string]string{"type": "integer", "format": "int64"}
			}
			operation["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object", "additionalProperties": false, "properties": properties}}}}
		}
		methods[method] = operation
	}
	return json.MarshalIndent(document, "", "  ")
}
