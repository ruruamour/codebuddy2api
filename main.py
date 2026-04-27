from codebuddy2api.app import create_app
from codebuddy2api.config import Settings

app = create_app()


if __name__ == "__main__":
    import uvicorn

    settings = Settings.from_env()
    uvicorn.run(
        "main:app",
        host=settings.host,
        port=settings.port,
        reload=False,
        log_level=settings.log_level.lower(),
    )
