"""Addon mitmproxy de Mint.

Ce fichier est embarqué dans le binaire Go puis extrait dans un répertoire
temporaire au démarrage du proxy. Il envoie les événements HTTP au pont local
de CRL-CLI, sur 127.0.0.1, avec un jeton de session fourni par l'application.

Aucun secret n'est journalisé. Le pont est contacté en direct avec
http.client : les variables de proxy de l'environnement ne sont pas utilisées,
ce qui évite toute boucle de capture.
"""

from __future__ import annotations

import base64
import http.client
import json
import os
import queue
import threading
import time
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional

try:
    from mitmproxy import ctx
except Exception:  # pragma: no cover - permet la compilation syntaxique hors mitmproxy
    ctx = None  # type: ignore


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _env_int(name: str, default: int) -> int:
    try:
        value = int(os.environ.get(name, ""))
        return value if value > 0 else default
    except (TypeError, ValueError):
        return default


def _log(level: str, message: str) -> None:
    if ctx is None:
        return
    try:
        getattr(ctx.log, level)(message)
    except Exception:
        pass


class _Sender(threading.Thread):
    """Envoie les événements hors du thread principal de mitmproxy."""

    def __init__(self, host: str, port: int, token: str, timeout: float, queue_size: int):
        super().__init__(name="mint-bridge-sender", daemon=True)
        self.host = host
        self.port = port
        self.token = token
        self.timeout = timeout
        self.queue: "queue.Queue[Optional[Dict[str, Any]]]" = queue.Queue(maxsize=queue_size)
        self.dropped = 0
        self._warned = 0

    def emit(self, payload: Dict[str, Any]) -> None:
        try:
            self.queue.put_nowait(payload)
        except queue.Full:
            self.dropped += 1
            if self.dropped in (1, 10, 100):
                _log("warn", f"Mint : file de notifications pleine ({self.dropped} événements ignorés)")

    def post(self, payload: Dict[str, Any]) -> bool:
        data = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        conn = None
        try:
            conn = http.client.HTTPConnection(self.host, self.port, timeout=self.timeout)
            conn.request(
                "POST",
                "/events",
                body=data,
                headers={
                    "Authorization": "Bearer " + self.token,
                    "Content-Type": "application/json",
                    "Content-Length": str(len(data)),
                    "Connection": "close",
                },
            )
            response = conn.getresponse()
            response.read()
            if response.status not in (202, 204):
                self._warn(f"Mint : pont local a répondu HTTP {response.status}")
                return False
            return True
        except Exception as exc:
            self._warn(f"Mint : envoi au pont impossible ({exc.__class__.__name__})")
            return False
        finally:
            if conn is not None:
                try:
                    conn.close()
                except Exception:
                    pass

    def _warn(self, message: str) -> None:
        self._warned += 1
        if self._warned in (1, 10, 100):
            _log("warn", message)

    def run(self) -> None:
        while True:
            payload = self.queue.get()
            try:
                if payload is None:
                    return
                self.post(payload)
            finally:
                self.queue.task_done()

    def flush(self, timeout: float = 2.0) -> None:
        try:
            self.queue.put(None, timeout=timeout)
        except queue.Full:
            return
        self.join(timeout=timeout)


class MintAddon:
    def __init__(self) -> None:
        self.host = os.environ.get("MINT_BRIDGE_HOST", "127.0.0.1")
        self.port = _env_int("MINT_BRIDGE_PORT", 0)
        self.token = os.environ.get("MINT_BRIDGE_TOKEN", "")
        self.max_body = _env_int("MINT_MAX_BODY_BYTES", 2 << 20)
        self.timeout = float(_env_int("MINT_BRIDGE_TIMEOUT_MS", 2000)) / 1000.0
        self.queue_size = _env_int("MINT_EVENT_QUEUE", 4096)
        self.sender: Optional[_Sender] = None
        self.enabled = bool(self.port and self.token)

    def load(self, loader) -> None:  # noqa: ANN001 - signature mitmproxy
        if not self.enabled:
            _log("error", "Mint : configuration du pont local absente ; capture désactivée")
            return
        self.sender = _Sender(
            self.host,
            self.port,
            self.token,
            self.timeout,
            self.queue_size,
        )
        self.sender.start()

    def running(self) -> None:
        if self.sender is None:
            return
        # Envoi direct : confirme au plus vite que le proxy et l'addon sont prêts.
        self.sender.post(
            {
                "type": "ready",
                "version": 1,
                "timestamp": _now_iso(),
                "message": "addon chargé",
            }
        )

    def request(self, flow) -> None:  # noqa: ANN001
        if self.sender is None:
            return
        try:
            request = flow.request
            body, body_size, truncated = self._body(request)
            headers = self._headers(request)
            payload = {
                "type": "request",
                "flow_id": str(flow.id),
                "timestamp": _now_iso(),
                "method": str(request.method),
                "url": str(request.pretty_url),
                "http_version": str(getattr(request, "http_version", "")),
                "headers": headers,
                "body_b64": base64.b64encode(body).decode("ascii"),
                "body_size": body_size,
                "body_truncated": truncated,
            }
            self.sender.emit(payload)
        except Exception as exc:
            _log("warn", f"Mint : capture de requête impossible ({exc.__class__.__name__})")

    def response(self, flow) -> None:  # noqa: ANN001
        if self.sender is None:
            return
        try:
            response = flow.response
            if response is None:
                return
            body, body_size, truncated = self._body(response)
            duration_ms = self._duration_ms(flow)
            payload = {
                "type": "response",
                "flow_id": str(flow.id),
                "timestamp": _now_iso(),
                "status_code": int(response.status_code),
                "reason": str(getattr(response, "reason", "") or ""),
                "http_version": str(getattr(response, "http_version", "")),
                "headers": self._headers(response),
                "body_b64": base64.b64encode(body).decode("ascii"),
                "body_size": body_size,
                "body_truncated": truncated,
                "duration_ms": duration_ms,
            }
            self.sender.emit(payload)
        except Exception as exc:
            _log("warn", f"Mint : capture de réponse impossible ({exc.__class__.__name__})")

    def error(self, flow) -> None:  # noqa: ANN001
        if self.sender is None:
            return
        message = "erreur de transport inconnue"
        try:
            if getattr(flow, "error", None) is not None:
                message = str(flow.error.msg)
        except Exception:
            pass
        self.sender.emit(
            {
                "type": "error",
                "flow_id": str(flow.id),
                "timestamp": _now_iso(),
                "error": message[:8000],
            }
        )

    def done(self) -> None:
        if self.sender is not None:
            self.sender.flush()

    def _headers(self, message) -> List[Dict[str, str]]:  # noqa: ANN001
        try:
            items = message.headers.items(multi=True)
        except TypeError:
            items = message.headers.items()
        return [{"name": str(name), "value": str(value)} for name, value in items]

    def _body(self, message) -> tuple[bytes, int, bool]:  # noqa: ANN001
        try:
            content = getattr(message, "raw_content", None)
            if content is None:
                content = message.get_content(strict=False)
            if content is None:
                return b"", 0, False
            size = len(content)
            if size > self.max_body:
                return content[: self.max_body], size, True
            return content, size, False
        except Exception:
            # Le corps n'a pas pu être lu : on marque explicitement la capture
            # comme incomplète plutôt que de laisser croire qu'elle est vide.
            return b"", 0, True

    def _duration_ms(self, flow) -> float:  # noqa: ANN001
        try:
            end = flow.response.timestamp_end
            start = flow.request.timestamp_start
            if end is None or start is None:
                return 0.0
            return max(0.0, (float(end) - float(start)) * 1000.0)
        except Exception:
            return 0.0


addons = [MintAddon()]
