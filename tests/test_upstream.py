from codebuddy2api.upstream import StreamState, build_non_stream_response, normalize_chunk_for_client, parse_sse_data_line


def test_parse_sse_data_line():
    assert parse_sse_data_line("data: [DONE]") == "[DONE]"
    assert parse_sse_data_line("data: {\"x\":1}") == '{"x":1}'
    assert parse_sse_data_line(": keepalive") is None
    assert parse_sse_data_line("") is None


def test_prepare_payload_strips_openai_reasoning_control():
    from codebuddy2api.config import Settings
    from codebuddy2api.upstream import CodeBuddyClient

    client = CodeBuddyClient(
        Settings(
            host="127.0.0.1",
            port=18182,
            db_path=":memory:",
            api_key="",
            admin_key="",
            upstream_url="https://example.invalid/v2/chat/completions",
            models=("glm-5.1",),
            cooldown_seconds=300,
            failure_threshold=3,
            default_concurrency=1,
            request_timeout_seconds=300,
            connect_timeout_seconds=10,
            log_level="INFO",
        )
    )
    payload = client.prepare_payload({"model": "glm-5.1", "stream": False, "reasoning_effort": "xhigh"})
    assert payload["stream"] is True
    assert "reasoning_effort" not in payload


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


def test_normalize_chunk_promotes_reasoning_when_content_is_empty():
    state = StreamState(response_id="chatcmpl-test", model="glm-5.1")
    chunk = {"choices": [{"delta": {"content": "", "reasoning_content": "OK"}}]}
    normalize_chunk_for_client(chunk, state)
    assert chunk["choices"][0]["delta"]["content"] == "OK"
    assert state.reasoning_parts == ["OK"]
    assert build_non_stream_response(state)["choices"][0]["message"]["content"] == "OK"


def test_build_non_stream_response():
    state = StreamState(response_id="chatcmpl-test", model="glm-5.1")
    state.content_parts.extend(["O", "K"])
    state.usage = {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
    response = build_non_stream_response(state)
    assert response["choices"][0]["message"]["content"] == "OK"
    assert response["usage"]["total_tokens"] == 2
