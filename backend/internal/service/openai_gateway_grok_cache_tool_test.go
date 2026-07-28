package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAppendMissingGrokFreeCacheNativeTools_PureClientFunctionInjects(t *testing.T) {
	body := []byte(`{
		"model": "grok-4.5",
		"tools": [
			{"type":"function","name":"view_image","description":"View image","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}
		],
		"tool_choice": "auto"
	}`)

	result, err := appendMissingGrokFreeCacheNativeTools(body)
	require.NoError(t, err)

	tools := gjson.GetBytes(result, "tools").Array()
	require.Len(t, tools, 3)
	require.Equal(t, "view_image", tools[0].Get("name").String())
	require.Equal(t, "web_search", tools[1].Get("type").String())
	require.Equal(t, "x_search", tools[2].Get("type").String())
}

func TestAppendMissingGrokFreeCacheNativeTools_FunctionPlusWebSearchInjects(t *testing.T) {
	body := []byte(`{
		"model": "grok-4.5",
		"tools": [
			{"type":"function","name":"view_image","description":"View","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
			{"type":"function","name":"web_search","description":"Search","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}
		]
	}`)

	result, err := appendMissingGrokFreeCacheNativeTools(body)
	require.NoError(t, err)

	tools := gjson.GetBytes(result, "tools").Array()
	types := make(map[string]bool)
	names := make(map[string]bool)
	for _, tool := range tools {
		types[tool.Get("type").String()] = true
		if n := tool.Get("name").String(); n != "" {
			names[n] = true
		}
	}
	assert.True(t, types["web_search"], "web_search should be present (converted from function)")
	assert.True(t, types["x_search"], "x_search should be injected when missing")
	assert.True(t, names["view_image"], "client tools should be preserved")
	// function-form web_search should be promoted away, not left as a function
	for _, tool := range tools {
		if tool.Get("type").String() == "function" {
			assert.NotEqual(t, "web_search", tool.Get("name").String())
		}
	}
}

func TestAppendMissingGrokFreeCacheNativeTools_NativeSearchAlreadyPresent(t *testing.T) {
	body := []byte(`{
		"model": "grok-4.5",
		"tools": [
			{"type":"function","name":"view_image","description":"View","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
			{"type":"web_search"}
		]
	}`)

	result, err := appendMissingGrokFreeCacheNativeTools(body)
	require.NoError(t, err)

	tools := gjson.GetBytes(result, "tools").Array()
	types := make(map[string]bool)
	for _, tool := range tools {
		types[tool.Get("type").String()] = true
	}
	assert.True(t, types["web_search"])
	assert.True(t, types["x_search"], "x_search should be injected when web_search is already present")
}

func TestAppendMissingGrokFreeCacheNativeTools_MultipleFunctionsInjectsSearch(t *testing.T) {
	body := []byte(`{
		"model": "grok-4.5",
		"tools": [
			{"type":"function","name":"view_image","description":"View","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}},
			{"type":"function","name":"read_file","description":"Read","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}
		]
	}`)

	result, err := appendMissingGrokFreeCacheNativeTools(body)
	require.NoError(t, err)

	tools := gjson.GetBytes(result, "tools").Array()
	require.Len(t, tools, 4, "missing native search tools should be injected")
	require.Equal(t, "web_search", tools[2].Get("type").String())
	require.Equal(t, "x_search", tools[3].Get("type").String())
}

func TestEnsureGrokNativeSearchTools_EmptyBodyGetsSearchOnly(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":"hello"}`)
	result, err := ensureGrokNativeSearchTools(body)
	require.NoError(t, err)
	tools := gjson.GetBytes(result, "tools").Array()
	require.Len(t, tools, 2)
	require.Equal(t, "web_search", tools[0].Get("type").String())
	require.Equal(t, "x_search", tools[1].Get("type").String())
}
