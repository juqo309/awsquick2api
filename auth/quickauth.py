#!/usr/bin/env python3
"""Native Amazon Quick (QuickWork) login helper.

Implements the desktop OAuth flow in Python. No official desktop app and no
outpost process are involved:

    discovery URL -> browser login -> localhost callback (port 18080)
    -> PKCE authorize -> token exchange -> data/session.json

The URL printed by `upstream-login` is generated locally; the account/tenant is
resolved by Amazon during the browser login, never guessed here.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import html
import json
import os
import re
import secrets
import ssl
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Optional

APP_VERSION = "1.2.4660"
DEFAULT_PORT = 18080
LOGIN_TIMEOUT_S = 60 * 60

ROOT = Path(__file__).resolve().parent.parent
DATA_DIR = ROOT / "data"
SESSION_FILE = DATA_DIR / "session.json"
UPSTREAM_FILE = DATA_DIR / "upstream.json"


def bind_data_dir(path: Optional[str] = None) -> None:
    global DATA_DIR, SESSION_FILE, UPSTREAM_FILE
    raw = (path or os.environ.get("QW_DATA_DIR") or "").strip()
    DATA_DIR = Path(raw) if raw else (ROOT / "data")
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    SESSION_FILE = DATA_DIR / "session.json"
    UPSTREAM_FILE = DATA_DIR / "upstream.json"


def apply_proxy(proxy: str) -> None:
    proxy = (proxy or os.environ.get("QW_PROXY") or "").strip()
    if not proxy:
        return
    os.environ["HTTP_PROXY"] = proxy
    os.environ["HTTPS_PROXY"] = proxy
    os.environ["ALL_PROXY"] = proxy

SURFACE = os.environ.get("QW_SURFACE", "").strip() or "QUICK_WORK_DESKTOP_WINDOWS"
GATEWAY_WS_PATH = "/ws/service/SpaceNeedleConversationService/operation/ChatBidirectionalStream"

SOCIAL_DISCOVERY = {
    "prod": "https://quick.aws.com/sn",
    "beta": "https://devtest.quicksuite.aws.dev/sn",
    "integ": "https://devtest.quicksuite.aws.dev/sn",
    "dev": "https://devtest.quicksuite.aws.dev/sn",
}
SOCIAL_DATA_PLANE = {
    "prod": "https://cell001.dp.appintegrations.us-east-1.prod.plato.ai.aws.dev",
    "beta": "https://cell001.dp.appintegrations.us-west-2.beta.plato.ai.aws.dev",
    "integ": "https://cell001.dp.appintegrations.us-west-2.beta.plato.ai.aws.dev",
    "dev": "https://cell001.dp.appintegrations.us-west-2.beta.plato.ai.aws.dev",
}
SOCIAL_SCOPES = {
    "prod": ["quicksight:read_write"],
    "beta": ["quicksight_test:read_write"],
    "integ": ["quicksight_test:read_write"],
    "dev": ["quicksight_test:read_write"],
}
ENTERPRISE_DISCOVERY = {
    "prod": "https://quicksight.aws.amazon.com/sn/start/app-integrations/quickwork-enterprise-login",
    "beta": "https://spaceneedle-alpha.amazon.com/sn/start/app-integrations/quickwork-enterprise-login",
    "integ": "https://spaceneedle-beta.amazon.com/sn/start/app-integrations/quickwork-enterprise-login",
    "dev": "https://spaceneedle-alpha.amazon.com/sn/start/app-integrations/quickwork-enterprise-login",
}
ENTERPRISE_SCOPE = "openid profile email"
GATEWAY_PROVIDERS = {"gateway", "social_gateway"}


def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).decode("ascii").rstrip("=")


def _pkce() -> tuple[str, str]:
    verifier = _b64url(secrets.token_bytes(32))
    challenge = _b64url(hashlib.sha256(verifier.encode("ascii")).digest())
    return verifier, challenge


def _decode_jwt(token: str) -> dict[str, Any]:
    try:
        payload = token.split(".")[1]
        payload += "=" * (-len(payload) % 4)
        parsed = json.loads(base64.urlsafe_b64decode(payload.encode("ascii")))
        return parsed if isinstance(parsed, dict) else {}
    except Exception:
        return {}


def _http_post(url: str, body: bytes, headers: dict[str, str], timeout: float = 30.0) -> tuple[int, bytes]:
    req = urllib.request.Request(url, data=body, headers=headers, method="POST")
    ctx = ssl.create_default_context()
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read()


def _post_json(url: str, payload: dict[str, Any], headers: Optional[dict[str, str]] = None) -> dict[str, Any]:
    body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
    hdrs = {"Content-Type": "application/json", "Accept": "application/json"}
    if headers:
        hdrs.update(headers)
    status, raw = _http_post(url, body, hdrs)
    if status < 200 or status >= 300:
        raise RuntimeError(f"HTTP {status} from {urllib.parse.urlsplit(url).netloc}: {_safe_body(raw)}")
    try:
        parsed = json.loads(raw.decode("utf-8"))
    except Exception as exc:
        raise RuntimeError(f"invalid JSON from {urllib.parse.urlsplit(url).netloc}: {exc}") from exc
    if not isinstance(parsed, dict):
        raise RuntimeError(f"unexpected JSON from {urllib.parse.urlsplit(url).netloc}")
    return parsed


def _post_form(url: str, form: dict[str, str], headers: Optional[dict[str, str]] = None) -> dict[str, Any]:
    body = urllib.parse.urlencode(form).encode("utf-8")
    hdrs = {"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"}
    if headers:
        hdrs.update(headers)
    status, raw = _http_post(url, body, hdrs)
    if status < 200 or status >= 300:
        raise RuntimeError(f"HTTP {status} from {urllib.parse.urlsplit(url).netloc}: {_safe_body(raw)}")
    try:
        parsed = json.loads(raw.decode("utf-8"))
    except Exception as exc:
        raise RuntimeError(f"invalid JSON from {urllib.parse.urlsplit(url).netloc}: {exc}") from exc
    if not isinstance(parsed, dict):
        raise RuntimeError(f"unexpected JSON from {urllib.parse.urlsplit(url).netloc}")
    return parsed


def _safe_body(raw: bytes) -> str:
    text = raw.decode("utf-8", "replace").strip()
    return text[:300]


def _one_line(value: Any) -> str:
    return " ".join(str(value).split())


def _write_private_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    with open(tmp, "w", encoding="utf-8", newline="\n") as fh:
        json.dump(value, fh, ensure_ascii=False, indent=2)
        fh.write("\n")
    try:
        os.chmod(tmp, 0o600)
    except OSError:
        pass
    os.replace(tmp, path)
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass


def _load_json(path: Path) -> dict[str, Any]:
    with open(path, "r", encoding="utf-8") as fh:
        value = json.load(fh)
    if not isinstance(value, dict):
        raise RuntimeError(f"{path} does not contain a JSON object")
    return value


@dataclass
class Params:
    provider: str
    port: int
    stage: str
    region: str
    state: str
    verifier: str
    challenge: str
    nonce: str
    redirect_uri: str = ""
    discovery: dict[str, Any] = field(default_factory=dict)
    client_id: str = ""
    client_secret: str = ""
    tokens: dict[str, Any] = field(default_factory=dict)
    user_info: dict[str, Any] = field(default_factory=dict)
    tenant_url: str = ""
    error: str = ""

    def __post_init__(self) -> None:
        if not self.redirect_uri:
            if self.provider in ("enterprise", "gateway"):
                self.redirect_uri = f"http://localhost:{self.port}"
            else:
                self.redirect_uri = f"http://127.0.0.1:{self.port}"


class FlowState:
    def __init__(self, params: Params) -> None:
        self.params = params
        self.done = threading.Event()
        self.lock = threading.Lock()
        self.callback_lock = threading.Lock()
        self.discovered = False
        self.finished = False

    def finish(self) -> None:
        with self.lock:
            if self.finished:
                return
            self.finished = True
        self.done.set()


class LoginHandler(BaseHTTPRequestHandler):
    server_version = "quickauth/1.0"

    def log_message(self, fmt: str, *args: Any) -> None:  # noqa: A003 - stdlib signature
        return

    @property
    def state(self) -> FlowState:
        return self.server.flow  # type: ignore[attr-defined]

    def do_GET(self) -> None:  # noqa: N802 - stdlib signature
        parsed = urllib.parse.urlsplit(self.path)
        query = urllib.parse.parse_qs(parsed.query, keep_blank_values=True)

        if parsed.path == "/favicon.ico":
            self.send_response(204)
            self.end_headers()
            return
        if parsed.path in ("/callback", "/", "/index.html"):
            with self.state.callback_lock:
                if self.state.finished:
                    self._reply(409, _error_html("Sign-in has already finished."))
                elif parsed.path == "/callback":
                    self.handle_discovery(query)
                else:
                    self.handle_code(query)
            return
        self.send_response(404)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.end_headers()
        self.wfile.write(b"not found")

    def _reply(self, status: int, body: str, content_type: str = "text/html; charset=utf-8") -> None:
        data = body.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def _redirect(self, url: str) -> None:
        self.send_response(302)
        self.send_header("Location", url)
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def handle_discovery(self, query: dict[str, list[str]]) -> None:
        p = self.state.params
        got_nonce = _first(query, "nonce")
        if not got_nonce or not secrets.compare_digest(got_nonce.encode(), p.nonce.encode()):
            self._reply(400, _error_html("Discovery nonce mismatch"))
            return
        if self.state.discovered:
            self._reply(400, _error_html("Discovery is already complete. Use the latest sign-in link."))
            return

        err = _first(query, "error")
        if err:
            self._fail(f"Login failed: {err} {_first(query, 'error_description')}".strip(), status=400)
            return

        if self.headers.get("X-Quick-Callback-Relay") == "1" and not valid_relay_discovery(query):
            self._reply(400, _error_html("Unexpected Quick discovery endpoint"))
            return

        if p.provider in ("enterprise", "gateway"):
            p.discovery = {
                "auth_endpoint": _q(query, "authEndpoint", "auth_endpoint"),
                "token_endpoint": _q(query, "tokenEndpoint", "token_endpoint"),
                "issuer": _q(query, "issuer"),
                "client_id": _q(query, "clientId", "client_id"),
                "client_secret": _q(query, "clientSecret", "client_secret"),
                "tenant_url": _q(query, "tenantUrl", "tenant_url"),
                "region": _q(query, "region") or p.region,
                "account": _q(query, "account"),
                "namespace": _q(query, "namespace"),
            }
            if not p.discovery["auth_endpoint"] or not p.discovery["token_endpoint"]:
                self._fail("Discovery response is missing auth_endpoint/token_endpoint", status=400)
                return
            p.tenant_url = p.discovery["tenant_url"]
            authorize_url = self._enterprise_authorize_url(p.discovery)
            print(f"[quickauth] discovery: tenant={p.tenant_url or '-'} region={p.discovery['region'] or '-'}", flush=True)
            self.state.discovered = True
            self._redirect(authorize_url)
            return

        p.discovery = {
            "issuer_url": _q(query, "issuerUrl", "issuer_url", "issuer"),
            "tenant_url": _q(query, "tenantUrl", "tenant_url"),
            "account": _q(query, "account"),
            "idc_instance_id": _q(query, "idcInstanceId", "idc_instance_id"),
            "idc_region": _q(query, "idcRegion", "idc_region"),
        }
        p.tenant_url = p.discovery["tenant_url"]
        if not p.discovery["issuer_url"]:
            self._fail("Discovery callback did not provide issuerUrl", status=400)
            return
        idc_region = p.discovery["idc_region"] or p.region or "us-east-1"
        try:
            client = self._register_idc_client(idc_region)
        except Exception as exc:
            self._fail(f"Client registration failed: {exc}", status=502)
            return
        p.client_id = str(client.get("clientId") or client.get("client_id") or "")
        p.client_secret = str(client.get("clientSecret") or client.get("client_secret") or "")
        if not p.client_id:
            self._fail("Client registration returned no clientId", status=502)
            return
        p.discovery["idc_region"] = idc_region
        print(f"[quickauth] discovery: tenant={p.tenant_url or '-'} idc_region={idc_region}", flush=True)
        self.state.discovered = True
        self._redirect(self._social_authorize_url(idc_region))

    def _register_idc_client(self, idc_region: str) -> dict[str, Any]:
        p = self.state.params
        payload = {
            "clientName": "AmazonQuick-Desktop",
            "clientType": "public",
            "scopes": SOCIAL_SCOPES.get(p.stage, SOCIAL_SCOPES["prod"]),
            "grantTypes": ["authorization_code", "refresh_token"],
            "redirectUris": [p.redirect_uri],
            "issuerUrl": p.discovery.get("issuer_url") or "",
        }
        if not payload["issuerUrl"]:
            raise RuntimeError("discovery callback did not provide issuerUrl")
        return _post_json(f"https://oidc.{idc_region}.amazonaws.com/client/register", payload)

    def _social_authorize_url(self, idc_region: str) -> str:
        p = self.state.params
        params = {
            "response_type": "code",
            "client_id": p.client_id,
            "redirect_uri": p.redirect_uri,
            "scope": " ".join(SOCIAL_SCOPES.get(p.stage, SOCIAL_SCOPES["prod"])),
            "state": p.state,
            "code_challenge": p.challenge,
            "code_challenge_method": "S256",
        }
        return f"https://oidc.{idc_region}.amazonaws.com/authorize?{urllib.parse.urlencode(params)}"

    def _enterprise_authorize_url(self, discovery: dict[str, Any]) -> str:
        p = self.state.params
        params = {
            "response_type": "code",
            "client_id": discovery.get("client_id", ""),
            "redirect_uri": p.redirect_uri,
            "scope": ENTERPRISE_SCOPE,
            "state": p.state,
            "code_challenge": p.challenge,
            "code_challenge_method": "S256",
            "access_type": "offline",
            "prompt": "consent",
        }
        return f"{discovery['auth_endpoint']}?{urllib.parse.urlencode(params)}"

    def handle_code(self, query: dict[str, list[str]]) -> None:
        p = self.state.params
        got_state = _first(query, "state")
        if not got_state or not secrets.compare_digest(got_state.encode(), p.state.encode()) or not self.state.discovered:
            self._reply(400, _error_html("OAuth callback does not match this sign-in"))
            return
        err = _first(query, "error")
        if err:
            self._fail(f"Login failed: {err} {_first(query, 'error_description')}".strip(), status=400)
            return
        code = _first(query, "code")
        if not code:
            self._reply(400, _error_html("OAuth callback has no code"))
            return

        try:
            if p.provider in ("enterprise", "gateway"):
                p.tokens = self._exchange_enterprise_code(code)
            else:
                p.tokens = self._exchange_idc_code(code)
            p.user_info = self._fetch_user_info()
            self._save()
        except Exception as exc:
            self._fail(f"Token exchange failed: {exc}", status=502)
            return

        display = p.user_info.get("email") or p.user_info.get("user_name") or p.user_info.get("display_name") or ""
        print(f"[quickauth] LOGIN_OK {display}".rstrip(), flush=True)
        print(f"[quickauth] SESSION_SAVED {SESSION_FILE}", flush=True)
        self._reply(200, _success_html(display))
        self.state.finish()

    def _exchange_idc_code(self, code: str) -> dict[str, Any]:
        p = self.state.params
        idc_region = p.discovery.get("idc_region") or p.region or "us-east-1"
        payload = {
            "clientId": p.client_id,
            "clientSecret": p.client_secret,
            "grantType": "authorization_code",
            "code": code,
            "redirectUri": p.redirect_uri,
            "codeVerifier": p.verifier,
        }
        raw = _post_json(f"https://oidc.{idc_region}.amazonaws.com/token", payload)
        return {
            "access_token": raw.get("accessToken") or raw.get("access_token") or "",
            "refresh_token": raw.get("refreshToken") or raw.get("refresh_token") or "",
            "id_token": raw.get("idToken") or raw.get("id_token") or "",
            "expires_in": raw.get("expiresIn") or raw.get("expires_in") or 0,
        }

    def _exchange_enterprise_code(self, code: str) -> dict[str, Any]:
        p = self.state.params
        d = p.discovery
        form = {
            "grant_type": "authorization_code",
            "code": code,
            "redirect_uri": p.redirect_uri,
            "client_id": d.get("client_id", ""),
            "code_verifier": p.verifier,
        }
        if d.get("client_secret"):
            form["client_secret"] = d["client_secret"]
        raw = _post_form(d["token_endpoint"], form)
        return {
            "access_token": raw.get("access_token") or raw.get("accessToken") or "",
            "refresh_token": raw.get("refresh_token") or raw.get("refreshToken") or "",
            "id_token": raw.get("id_token") or raw.get("idToken") or "",
            "expires_in": raw.get("expires_in") or raw.get("expiresIn") or 0,
        }

    def _user_info_base(self) -> str:
        p = self.state.params
        if p.provider in ("enterprise", "gateway"):
            base = p.discovery.get("tenant_url") or p.tenant_url
            if not base:
                raise RuntimeError("discovery response has no tenant_url")
            return base
        tenant = (p.discovery.get("tenant_url") or "").rstrip("/")
        if "dp.appintegrations" in tenant:
            return tenant
        return SOCIAL_DATA_PLANE.get(p.stage, SOCIAL_DATA_PLANE["prod"])

    def _fetch_user_info(self) -> dict[str, Any]:
        p = self.state.params
        access_token = p.tokens.get("access_token") or ""
        if not access_token:
            raise RuntimeError("token response has no access_token")
        base = self._user_info_base().rstrip("/")
        headers = {
            "Authorization": f"Bearer {access_token}",
            "Content-Type": "application/json",
            "Accept": "application/json",
        }
        raw = _post_json(f"{base}/integration/quick-work/user-info", {}, headers)
        return normalize_user_info(raw)

    def _save(self) -> None:
        save_session(self.state.params)

    def _fail(self, message: str, status: int = 500) -> None:
        print(f"[quickauth] LOGIN_FAILED {message}", flush=True)
        self.state.params.error = message
        self._reply(status, _error_html(message))
        self.state.finish()


def valid_relay_discovery(query: dict[str, list[str]]) -> bool:
    region = _q(query, "idcRegion", "idc_region")
    if region and not re.fullmatch(r"[a-z]{2}(?:-[a-z]+)+-[0-9]+", region):
        return False
    # Never contact a user-supplied, arbitrary token or user-info endpoint.
    for names in (("authEndpoint", "auth_endpoint"), ("tokenEndpoint", "token_endpoint"), ("tenantUrl", "tenant_url")):
        value = _q(query, *names)
        if not value:
            continue
        try:
            parsed = urllib.parse.urlsplit(value)
            host = parsed.hostname or ""
            if parsed.scheme != "https" or parsed.username is not None or parsed.port not in (None, 443):
                return False
        except ValueError:
            return False
        if not (
            host in ("quick.aws.com", "quicksight.aws.amazon.com")
            or host.endswith((".quicksight.aws.amazon.com", ".quicksuite.aws.dev", ".awsapps.com"))
            or re.fullmatch(r"(?:oidc|sso)\.[a-z0-9-]+\.amazonaws\.com", host)
            or re.fullmatch(
                r"(?:cell[0-9]+|qbs-cell[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12})"
                r"\.dp\.appintegrations\.[a-z0-9-]+\.(?:prod|beta)\.plato\.ai\.aws\.dev",
                host,
            )
        ):
            return False
    return True


def _first(query: dict[str, list[str]], key: str) -> str:
    values = query.get(key)
    return values[0] if values else ""


def _q(query: dict[str, list[str]], *keys: str) -> str:
    """First non-empty value among aliases (the discovery callback uses camelCase)."""
    for key in keys:
        value = _first(query, key)
        if value:
            return value
    return ""


def _fmt_epoch(value: int) -> str:
    if not value:
        return "unknown"
    return time.strftime("%Y-%m-%d %H:%M:%S %Z", time.localtime(value))


def normalize_user_info(raw: dict[str, Any]) -> dict[str, Any]:
    if isinstance(raw.get("userInfo"), dict):
        raw = raw["userInfo"]

    def pick(*keys: str) -> str:
        for key in keys:
            value = raw.get(key)
            if value not in (None, ""):
                return _one_line(value)
        return ""

    out: dict[str, Any] = {
        "email": pick("email", "emailAddress"),
        "user_name": pick("userName", "user_name", "username"),
        "first_name": pick("firstName", "first_name"),
        "last_name": pick("lastName", "last_name"),
        "display_name": pick("displayName", "display_name"),
        "user_arn": pick("userArn", "user_arn"),
        "quicksight_external_user_arn": pick("quicksightExternalUserArn", "quicksight_external_user_arn"),
        "role": pick("role"),
        "identity_type": pick("identityType", "identity_type"),
        "aws_account_id": pick("awsAccountId", "aws_account_id", "accountId"),
        "account_name": pick("accountName", "account_name"),
        "external_id": pick("externalId", "external_id"),
        "group_arns_compressed": pick("groupArnsCompressed", "group_arns_compressed"),
        "account_ownership": pick("accountOwnership", "account_ownership"),
    }
    usage_summary = raw.get("usageSummary") if isinstance(raw.get("usageSummary"), dict) else raw.get("usage_summary")
    if isinstance(usage_summary, dict):
        out["usage_summary"] = usage_summary
    return out


def save_session(p: Params) -> dict[str, Any]:
    tenant_url = p.tenant_url or p.discovery.get("tenant_url") or ""
    if not tenant_url:
        tenant_url = SOCIAL_DATA_PLANE.get(p.stage, SOCIAL_DATA_PLANE["prod"])
    tenant_url = tenant_url.rstrip("/")
    if not urllib.parse.urlsplit(tenant_url).scheme:
        tenant_url = "https://" + tenant_url
    ws_host = urllib.parse.urlsplit(tenant_url).netloc
    if not ws_host:
        raise RuntimeError("cannot derive ws_host from tenant_url")

    access_token = p.tokens.get("access_token") or ""
    id_token = p.tokens.get("id_token") or ""
    jwt = id_token or access_token
    if not jwt:
        raise RuntimeError("token response has no id_token/access_token")
    claims = _decode_jwt(id_token) if id_token else {}
    actor_arn = p.user_info.get("user_arn") or ""
    if not actor_arn:
        sub = claims.get("sub")
        if isinstance(sub, str) and sub.startswith("arn:"):
            actor_arn = sub
    region = p.discovery.get("region") or p.region or claims.get("region") or "us-east-1"
    expires_in = int(p.tokens.get("expires_in") or 0)
    now = int(time.time())
    usable_until = now + expires_in if expires_in > 0 else 0
    if not usable_until:
        exp = claims.get("exp")
        if isinstance(exp, (int, float)):
            usable_until = int(exp)
    p.tenant_url = tenant_url

    session = {
        "jwt": jwt,
        "id_token": jwt,
        "ws_host": ws_host,
        "ws_path": GATEWAY_WS_PATH if p.provider in GATEWAY_PROVIDERS else "",
        "tenant_url": tenant_url,
        "provider": p.provider,
        "actor_arn": actor_arn,
        "surface": SURFACE,
        "aws_account_id": p.user_info.get("aws_account_id") or claims.get("aws_account_id") or "",
        "region": region,
        "account_name": p.user_info.get("account_name") or "",
        "embed_host": "",
        "usable_until": usable_until,
        "obtained_at": now,
    }
    upstream = {
        "version": 1,
        "provider": p.provider,
        "build_stage": p.stage,
        "region": region,
        "redirect_uri": p.redirect_uri,
        "tenant_url": tenant_url,
        "user_info": p.user_info,
        "tokens": {
            "access_token": access_token,
            "refresh_token": p.tokens.get("refresh_token") or "",
            "id_token": id_token,
            "expires_in": expires_in,
        },
        "client": {
            "client_id": p.client_id,
            "client_secret": p.client_secret,
            "idc_region": p.discovery.get("idc_region") or "",
            "issuer_url": p.discovery.get("issuer_url") or "",
        },
        "discovery": p.discovery,
        "obtained_at": now,
    }
    _write_private_json(SESSION_FILE, session)
    _write_private_json(UPSTREAM_FILE, upstream)
    print(f"[quickauth] provider={p.provider} ws_host={ws_host} expires={_fmt_epoch(usable_until)}", flush=True)
    return session


def _success_html(display: str) -> str:
    who = f"<p>{html.escape(display)}</p>" if display else ""
    return (
        "<!doctype html><html><head><meta charset='utf-8'><title>Login complete</title>"
        "<style>body{font-family:system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem}"
        "code{background:#eee;padding:.1rem .25rem}</style></head><body>"
        "<h2>Login complete</h2>"
        f"{who}"
        "<p>You can close this window and return to the terminal.</p>"
        "</body></html>"
    )


def _error_html(message: str) -> str:
    return (
        "<!doctype html><html><head><meta charset='utf-8'><title>Login failed</title></head><body>"
        "<h2>Login failed</h2>"
        f"<p>{html.escape(message)}</p>"
        "</body></html>"
    )


def _discovery_url(p: Params) -> str:
    surface = urllib.parse.quote(f"desktop:{p.nonce}:{p.port}:{APP_VERSION}", safe="")
    if p.provider in ("enterprise", "gateway"):
        base = ENTERPRISE_DISCOVERY.get(p.stage, ENTERPRISE_DISCOVERY["prod"])
        if p.region and p.region not in ("dynamic", "us-east-1"):
            parsed = urllib.parse.urlsplit(base)
            base = urllib.parse.urlunsplit((parsed.scheme, f"{p.region}.{parsed.netloc}", parsed.path, "", ""))
        return f"{base}?qwSurface={surface}"
    base = SOCIAL_DISCOVERY.get(p.stage, SOCIAL_DISCOVERY["prod"])
    return f"{base}?qwSurface={surface}"


def run_login(args: argparse.Namespace) -> int:
    provider = args.provider
    stage = args.build_stage
    verifier, challenge = _pkce()
    params = Params(
        provider=provider,
        port=args.port,
        stage=stage,
        region=args.region,
        state=_b64url(secrets.token_bytes(32)),
        verifier=verifier,
        challenge=challenge,
        nonce=_b64url(secrets.token_bytes(32)),
    )
    state = FlowState(params)
    try:
        server = ThreadingHTTPServer(("127.0.0.1", params.port), LoginHandler)
    except OSError as exc:
        print(f"[quickauth] cannot listen on 127.0.0.1:{params.port}: {exc}", file=sys.stderr)
        print(f"[quickauth] stop the process currently using port {params.port} and retry", file=sys.stderr)
        return 2
    server.daemon_threads = True
    server.flow = state  # type: ignore[attr-defined]
    thread = threading.Thread(target=server.serve_forever, name="quickauth-http", daemon=True)
    thread.start()

    url = _discovery_url(params)
    print(f"[quickauth] provider={provider} stage={stage} listening=127.0.0.1:{params.port}", flush=True)
    print("[quickauth] Open this URL to sign in:", flush=True)
    print(url, flush=True)
    print(f"[quickauth] waiting for login (timeout {LOGIN_TIMEOUT_S // 60} min)", flush=True)

    if args.open:
        import webbrowser

        threading.Thread(target=webbrowser.open, args=(url,), daemon=True).start()

    try:
        state.done.wait(timeout=args.timeout)
    except KeyboardInterrupt:
        print("[quickauth] cancelled", file=sys.stderr)
        server.shutdown()
        return 130
    finally:
        server.shutdown()

    if not state.finished:
        print("[quickauth] LOGIN_FAILED timed out waiting for browser callback", file=sys.stderr)
        return 1
    if params.error:
        return 1
    return 0


def _upstream_for_refresh() -> dict[str, Any]:
    if not UPSTREAM_FILE.exists():
        raise RuntimeError(f"{UPSTREAM_FILE} not found; run upstream-login first")
    return _load_json(UPSTREAM_FILE)


def cmd_refresh(args: argparse.Namespace) -> int:
    upstream = _upstream_for_refresh()
    provider = str(upstream.get("provider") or "")
    stage = str(upstream.get("build_stage") or "prod")
    region = str(upstream.get("region") or "us-east-1")
    redirect_uri = str(upstream.get("redirect_uri") or "")
    tenant_url = str(upstream.get("tenant_url") or "")
    tokens = upstream.get("tokens") if isinstance(upstream.get("tokens"), dict) else {}
    client = upstream.get("client") if isinstance(upstream.get("client"), dict) else {}
    discovery = upstream.get("discovery") if isinstance(upstream.get("discovery"), dict) else {}
    refresh_token = str(tokens.get("refresh_token") or "")
    if not refresh_token:
        print("[quickauth] no refresh_token stored; run upstream-login again", file=sys.stderr)
        return 1

    try:
        if provider in ("enterprise", "gateway"):
            form = {
                "grant_type": "refresh_token",
                "refresh_token": refresh_token,
                "client_id": str(discovery.get("client_id") or client.get("client_id") or ""),
            }
            secret = str(discovery.get("client_secret") or client.get("client_secret") or "")
            if secret:
                form["client_secret"] = secret
            raw = _post_form(str(discovery.get("token_endpoint") or ""), form)
            new_tokens = {
                "access_token": raw.get("access_token") or raw.get("accessToken") or "",
                "refresh_token": raw.get("refresh_token") or raw.get("refreshToken") or refresh_token,
                "id_token": raw.get("id_token") or raw.get("idToken") or "",
                "expires_in": raw.get("expires_in") or raw.get("expiresIn") or 0,
            }
        else:
            client_id = str(client.get("client_id") or "")
            idc_region = str(client.get("idc_region") or discovery.get("idc_region") or region or "us-east-1")
            payload = {
                "clientId": client_id,
                "clientSecret": str(client.get("client_secret") or ""),
                "grantType": "refresh_token",
                "refreshToken": refresh_token,
            }
            raw = _post_json(f"https://oidc.{idc_region}.amazonaws.com/token", payload)
            new_tokens = {
                "access_token": raw.get("accessToken") or raw.get("access_token") or "",
                "refresh_token": raw.get("refreshToken") or raw.get("refresh_token") or refresh_token,
                "id_token": raw.get("idToken") or raw.get("id_token") or "",
                "expires_in": raw.get("expiresIn") or raw.get("expires_in") or 0,
            }
        if not new_tokens["access_token"]:
            raise RuntimeError("refresh returned no access_token")

        if provider in ("enterprise", "gateway"):
            base = str(discovery.get("tenant_url") or tenant_url)
        else:
            dp = SOCIAL_DATA_PLANE.get(stage, SOCIAL_DATA_PLANE["prod"])
            base = tenant_url if "dp.appintegrations" in tenant_url else dp
        user_info = normalize_user_info(
            _post_json(
                f"{base.rstrip('/')}/integration/quick-work/user-info",
                {},
                {"Authorization": f"Bearer {new_tokens['access_token']}", "Content-Type": "application/json"},
            )
        )
        params = Params(
            provider=provider,
            port=DEFAULT_PORT,
            stage=stage,
            region=region,
            state="refresh",
            verifier="refresh",
            challenge="refresh",
            nonce="refresh",
            redirect_uri=redirect_uri or f"http://127.0.0.1:{DEFAULT_PORT}",
            discovery=discovery,
            client_id=str(client.get("client_id") or discovery.get("client_id") or ""),
            client_secret=str(client.get("client_secret") or discovery.get("client_secret") or ""),
            tokens=new_tokens,
            user_info=user_info,
            tenant_url=tenant_url,
        )
        save_session(params)
        return 0
    except Exception as exc:
        print(f"[quickauth] refresh failed: {exc}", file=sys.stderr)
        return 1


def cmd_status(args: argparse.Namespace) -> int:
    if not SESSION_FILE.exists():
        print("[quickauth] no session; run upstream-login")
        return 1
    session = _load_json(SESSION_FILE)
    print(f"provider    {session.get('provider', '')}")
    print(f"ws_host     {session.get('ws_host', '')}")
    print(f"tenant_url  {session.get('tenant_url', '')}")
    print(f"account     {session.get('aws_account_id', '')} {session.get('account_name', '')}".rstrip())
    print(f"region      {session.get('region', '')}")
    print(f"expires     {_fmt_epoch(int(session.get('usable_until') or 0))}")
    try:
        upstream = _upstream_for_refresh()
        user = upstream.get("user_info") if isinstance(upstream.get("user_info"), dict) else {}
        if user:
            print(f"user        {user.get('email', '')} {user.get('display_name', '')}".rstrip())
    except Exception:
        pass
    return 0


def cmd_logout(args: argparse.Namespace) -> int:
    removed = []
    for path in (SESSION_FILE, UPSTREAM_FILE):
        try:
            path.unlink()
            removed.append(path.name)
        except FileNotFoundError:
            pass
    print("[quickauth] removed " + (", ".join(removed) if removed else "nothing"))
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="quickauth.py", description=__doc__)
    parser.add_argument("--data-dir", default="", help="directory for session.json and upstream.json")
    parser.add_argument("--proxy", default="", help="optional HTTP(S) proxy for login requests")
    sub = parser.add_subparsers(dest="command", required=True)

    login = sub.add_parser("upstream-login", help="run the native browser OAuth login")
    login.add_argument("--provider", choices=["social", "enterprise", "gateway", "social_gateway"], default="social")
    login.add_argument("--build-stage", choices=["prod", "beta", "integ", "dev"], default="prod")
    login.add_argument("--region", default="", help="IDC region (social) or enterprise discovery region prefix")
    login.add_argument("--port", type=int, default=DEFAULT_PORT, help="localhost callback port (default 18080)")
    login.add_argument("--timeout", type=int, default=LOGIN_TIMEOUT_S, help="seconds to wait for the browser callback")
    login.add_argument("--open", action="store_true", help="also open the URL in the default browser")
    login.set_defaults(func=run_login)

    refresh = sub.add_parser("refresh", help="refresh tokens and rewrite data/session.json")
    refresh.set_defaults(func=cmd_refresh)

    status = sub.add_parser("status", help="show the stored session metadata")
    status.set_defaults(func=cmd_status)

    logout = sub.add_parser("logout", help="delete the stored session and refresh state")
    logout.set_defaults(func=cmd_logout)

    return parser


def main(argv: Optional[list[str]] = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    bind_data_dir(getattr(args, "data_dir", ""))
    apply_proxy(getattr(args, "proxy", ""))
    try:
        return int(args.func(args))
    except KeyboardInterrupt:
        return 130


if __name__ == "__main__":
    raise SystemExit(main())
