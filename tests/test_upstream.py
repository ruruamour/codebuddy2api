from codebuddy2api.upstream import StreamState, build_non_stream_response, normalize_chunk_for_client, parse_sse_data_line


def test_parse_sse_data_line():
    assert parse_sse_data_line("data: [DONE]") == "[DONE]"
    assert parse_sse_data_line("data: {\"x\":1}") == '{"x":1}'
    assert parse_sse_data_line(": keepalive") is None
    assert parse_sse_data_line("") is None


def test_normalize_chunk_collects_usage_and_content():
    state = StreamState(response_id="chatcmpl-test", model="glm-5.1")
    chunk = {
        "choices": [{"delta": {"content": "OK"}, "finish_reason": "stop"}],
        "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2, "credit": 0.01},
    }
    normalize_chunk_for_client(chunk, state)
    assert chunk["id"] == "chatcmpl-test"
    assert chunk["model"] == "glm-5.1"
    assert state.content_parts == ["OK"]
    assert state.finish_reason == "stop"
    assert state.usage["credit"] == 0.01


def test_build_non_stream_response():
    state = StreamState(response_id="chatcmpl-test", model="glm-5.1")
    state.content_parts.extend(["O", "K"])
    state.usage = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
    response = build_non_stream_response(state)
    assert response["choices"][0]["message"]["content"] == "OK"
    assert response["usage"]["total_tokens"] == 2
