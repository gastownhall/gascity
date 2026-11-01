#!/usr/bin/env python3
"""Exercise mcp-agent-mail's JWT decoder with the pinned JOSE stack."""

from __future__ import annotations

import asyncio
import importlib.metadata
import json
import time
from types import SimpleNamespace

from authlib.jose import JsonWebKey, jwt
from fastapi import FastAPI
from mcp_agent_mail.http import SecurityAndRateLimitMiddleware


def middleware(*, algorithms: list[str], secret: str | None = None,
               jwks_url: str | None = None, issuer: str | None = None,
               audience: str | None = None) -> SecurityAndRateLimitMiddleware:
    http_settings = SimpleNamespace(
        jwt_algorithms=algorithms,
        jwt_secret=secret,
        jwt_jwks_url=jwks_url,
        jwt_issuer=issuer,
        jwt_audience=audience,
        jwt_enabled=True,
    )
    return SecurityAndRateLimitMiddleware(FastAPI(), SimpleNamespace(http=http_settings))


def token(algorithm: str, claims: dict[str, object], key: object, *, kid: str | None = None) -> str:
    header: dict[str, str] = {"alg": algorithm}
    if kid:
        header["kid"] = kid
    return jwt.encode(header, claims, key).decode("utf-8")


async def serve_jwks(reader: asyncio.StreamReader, writer: asyncio.StreamWriter,
                    body: bytes) -> None:
    try:
        await reader.readuntil(b"\r\n\r\n")
        writer.write(
            b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: "
            + str(len(body)).encode("ascii")
            + b"\r\nConnection: close\r\n\r\n"
            + body
        )
        await writer.drain()
    finally:
        writer.close()
        await writer.wait_closed()


async def check_jwt_compatibility() -> None:
    installed = tuple(int(part) for part in importlib.metadata.version("joserfc").split(".")[:3])
    if installed < (1, 7, 7):
        raise AssertionError(f"image has joserfc {installed}, want at least 1.7.7")

    now = int(time.time())
    # The upstream service's existing JWT tests issue role-bearing tokens
    # without iss/aud when those checks are not configured. Keep that path.
    hmac_key = "container-scan-jwt-compatibility-secret"
    hmac_middleware = middleware(algorithms=["HS256"], secret=hmac_key)
    simple_token = token("HS256", {"sub": "smoke-user", "role": "reader"}, hmac_key)
    decoded = await hmac_middleware._decode_jwt(simple_token)
    if decoded is None or decoded.get("sub") != "smoke-user" or decoded.get("role") != "reader":
        raise AssertionError("valid HS256 role token without configured issuer/audience was rejected")
    if await hmac_middleware._decode_jwt("not.a.jwt") is not None:
        raise AssertionError("malformed JWT was accepted")

    # Explicit issuer and audience checks remain enforced after the JOSE update.
    claims = {
        "sub": "smoke-user",
        "role": "reader",
        "iss": "https://issuer.example.test",
        "aud": "mail-api",
        "exp": now + 60,
    }
    strict_middleware = middleware(
        algorithms=["HS256"],
        secret=hmac_key,
        issuer="https://issuer.example.test",
        audience="mail-api",
    )
    if await strict_middleware._decode_jwt(token("HS256", claims, hmac_key)) is None:
        raise AssertionError("valid issuer/audience constrained HS256 token was rejected")
    bad_issuer = dict(claims, iss="https://wrong.example.test")
    if await strict_middleware._decode_jwt(token("HS256", bad_issuer, hmac_key)) is not None:
        raise AssertionError("token with wrong configured issuer was accepted")
    bad_audience = dict(claims, aud="other-api")
    if await strict_middleware._decode_jwt(token("HS256", bad_audience, hmac_key)) is not None:
        raise AssertionError("token with wrong configured audience was accepted")

    # Exercise the service's JWKS fetch and RS256 verification path locally.
    private_jwk = JsonWebKey.generate_key("RSA", 2048, is_private=True).as_dict(is_private=True)
    private_jwk["kid"] = "mail-smoke-key"
    public_jwk = JsonWebKey.import_key(private_jwk).as_dict(is_private=False)
    public_jwk["kid"] = "mail-smoke-key"
    jwks = json.dumps({"keys": [public_jwk]}).encode("utf-8")
    async def handle_jwks(
        reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        await serve_jwks(reader, writer, jwks)

    server = await asyncio.start_server(handle_jwks, "127.0.0.1", 0)
    try:
        port = server.sockets[0].getsockname()[1]
        jwks_middleware = middleware(
            algorithms=["RS256"], jwks_url=f"http://127.0.0.1:{port}/jwks"
        )
        rs_token = token(
            "RS256",
            {"sub": "smoke-user", "role": "reader", "exp": now + 60},
            private_jwk,
            kid="mail-smoke-key",
        )
        rs_decoded = await jwks_middleware._decode_jwt(rs_token)
        if rs_decoded is None or rs_decoded.get("role") != "reader":
            raise AssertionError("valid JWKS-backed RS256 role token was rejected")
    finally:
        server.close()
        await server.wait_closed()


if __name__ == "__main__":
    asyncio.run(check_jwt_compatibility())
    print("MCP mail JWT compatibility passed with joserfc " + importlib.metadata.version("joserfc"))
