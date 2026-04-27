from fastapi.testclient import TestClient

from codebuddy2api.app import create_app
from codebuddy2api.config import Settings


def test_admin_ui_route_and_auth(tmp_path):
    settings = Settings(
        host="127.0.0.1",
        port=18182,
        db_path=str(tmp_path / "codebuddy2api.sqlite3"),
        api_key="client-key",
        admin_key="admin-key",
        upstream_url="https://example.invalid/v2/chat/completions",
        models=("glm-5.1",),
        cooldown_seconds=300,
        failure_threshold=3,
        default_concurrency=1,
        request_timeout_seconds=300,
        connect_timeout_seconds=10,
        log_level="INFO",
        debug_requests=False,
    )
    client = TestClient(create_app(settings), follow_redirects=False)

    root = client.get("/")
    assert root.status_code == 307
    assert root.headers["location"] == "/admin"

    admin = client.get("/admin")
    assert admin.status_code == 200
    assert "CodeBuddy2API Admin" in admin.text
    assert "localStorage" in admin.text

    assert client.get("/admin/stats").status_code == 401
    stats = client.get("/admin/stats", headers={"Authorization": "Bearer admin-key"})
    assert stats.status_code == 200
    assert stats.json()["accounts"] == 0
