"""Checked-in public model-price seed and cost projection helpers."""

from __future__ import annotations

import json
from decimal import Decimal, InvalidOperation
from pathlib import Path
from typing import Any

from .contract import normalize_timestamp
from .errors import ContractError, PricingError

PRICING_SEED_SCHEMA_VERSION = "1.0"
DEFAULT_PRICING_SEED = Path(__file__).with_name("model_pricing.json")
_PRICE_FIELDS = frozenset(
    {
        "model_id",
        "provider",
        "input_usd_per_million",
        "output_usd_per_million",
        "cache_read_usd_per_million",
        "cache_write_usd_per_million",
        "effective_from",
        "source",
    }
)


def _reject_json_constant(value: str) -> Any:
    raise ValueError(f"non-finite JSON constant {value!r} is not allowed")


def _required_text(row: dict[str, Any], field: str, where: str) -> str:
    value = row.get(field)
    if not isinstance(value, str) or not value.strip():
        raise PricingError(f"{where}.{field} must be a non-empty string")
    return value.strip()


def _price(value: Any, field: str, where: str, *, required: bool) -> Decimal | None:
    if value is None and not required:
        return None
    if isinstance(value, bool) or not isinstance(value, (Decimal, int, float, str)):
        nullable = " or null" if not required else ""
        raise PricingError(f"{where}.{field} must be a nonnegative decimal{nullable}")
    try:
        result = value if isinstance(value, Decimal) else Decimal(str(value))
    except (InvalidOperation, ValueError) as exc:
        raise PricingError(f"{where}.{field} must be a valid decimal") from exc
    if not result.is_finite():
        raise PricingError(f"{where}.{field} must be finite and nonnegative")
    if result < 0:
        raise PricingError(f"{where}.{field} must be nonnegative; negative prices are invalid")
    return result


def _decimal_text(value: Decimal | None) -> str | None:
    """Return a canonical, fixed-point decimal suitable for SQLite TEXT storage."""
    if value is None:
        return None
    if value.is_zero():
        return "0"
    text = format(value, "f")
    if "." in text:
        text = text.rstrip("0").rstrip(".")
    return text or "0"


def load_pricing_seed(path: str | Path | None = None) -> list[dict[str, Any]]:
    """Load and strictly validate the checked-in model-price seed JSON."""
    seed_path = DEFAULT_PRICING_SEED if path is None else Path(path)
    try:
        raw = json.loads(
            seed_path.read_text(encoding="utf-8"),
            parse_constant=_reject_json_constant,
            parse_float=Decimal,
        )
    except (OSError, ValueError) as exc:
        raise PricingError(f"cannot read pricing seed {seed_path}: {exc}") from exc
    if not isinstance(raw, dict) or set(raw) != {"schema_version", "models"}:
        raise PricingError("pricing seed must contain only schema_version and models")
    if raw.get("schema_version") != PRICING_SEED_SCHEMA_VERSION:
        raise PricingError(
            f"pricing seed schema_version must be {PRICING_SEED_SCHEMA_VERSION!r}"
        )
    models = raw.get("models")
    if not isinstance(models, list):
        raise PricingError("pricing seed models must be a list")

    normalized: list[dict[str, Any]] = []
    seen: set[tuple[str, str | None, str]] = set()
    for index, item in enumerate(models):
        where = f"models[{index}]"
        if not isinstance(item, dict):
            raise PricingError(f"{where} must be an object")
        unknown = sorted(set(item) - _PRICE_FIELDS)
        missing = sorted(_PRICE_FIELDS - set(item))
        if unknown or missing:
            details = []
            if missing:
                details.append("missing " + ", ".join(missing))
            if unknown:
                details.append("unknown " + ", ".join(unknown))
            raise PricingError(f"{where}: " + "; ".join(details))

        model_id = _required_text(item, "model_id", where)
        raw_provider = item["provider"]
        provider = (
            None if raw_provider is None else _required_text(item, "provider", where)
        )
        source = _required_text(item, "source", where)
        if not source.startswith("https://"):
            raise PricingError(f"{where}.source must be an HTTPS citation URL")
        try:
            effective_from = normalize_timestamp(item["effective_from"])
        except ContractError as exc:
            raise PricingError(
                f"{where}.effective_from must be timezone-aware ISO-8601"
            ) from exc
        key = (model_id, provider, effective_from)
        if key in seen:
            raise PricingError(f"{where} duplicates model/provider/effective_from {key!r}")
        seen.add(key)
        normalized.append(
            {
                "model_id": model_id,
                "provider": provider,
                "input_usd_per_million": _price(
                    item["input_usd_per_million"], "input_usd_per_million", where, required=True
                ),
                "output_usd_per_million": _price(
                    item["output_usd_per_million"], "output_usd_per_million", where, required=True
                ),
                "cache_read_usd_per_million": _price(
                    item["cache_read_usd_per_million"],
                    "cache_read_usd_per_million",
                    where,
                    required=False,
                ),
                "cache_write_usd_per_million": _price(
                    item["cache_write_usd_per_million"],
                    "cache_write_usd_per_million",
                    where,
                    required=False,
                ),
                "effective_from": effective_from,
                "source": source,
            }
        )
    return normalized


def seed_model_pricing(store: Any, path: str | Path | None = None) -> int:
    """Idempotently load the checked-in public prices into a schema-6 store."""
    return store.save_model_pricing(load_pricing_seed(path))


__all__ = [
    "DEFAULT_PRICING_SEED",
    "PRICING_SEED_SCHEMA_VERSION",
    "load_pricing_seed",
    "seed_model_pricing",
]
