#!/usr/bin/env python3
"""Writes passwords.json next to this file: password vectors for internal/auth.

usage: python3 -I internal/auth/testdata/make_passwords.py

The hashes come from Python hashlib, a second implementation next to the Go
code under test. Salts are derived from the vector name, so a second run gives
the same file byte for byte. Three public vectors are copied from the tests of
the old product; the script checks them before it writes anything.

Source: the public source of the old product, tag release-3.70.1-02, the test
DefaultSecurityPasswordServiceTest.java of its security component, read
2026-11-12. All other passwords are invented.
"""
from __future__ import annotations

import base64
import hashlib
import json
from pathlib import Path

OUT = Path(__file__).resolve().parent / "passwords.json"
PUBLIC = "the public source of the old product, tag release-3.70.1-02, DefaultSecurityPasswordServiceTest.java"
INVENTED = "invented, written by make_passwords.py with Python hashlib"
UNICODE = "Grüße ünd ß 密码 1"
SHIRO = {"SHA-512": "sha512", "SHA-384": "sha384", "SHA-256": "sha256", "SHA-1": "sha1", "MD5": "md5"}
PBKDF2 = {"PBKDF2WithHmacSHA1": "sha1", "PBKDF2WithHmacSHA256": "sha256"}


def b64(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")


def salt(name: str) -> bytes:
    return hashlib.sha256(("vector salt " + name).encode("utf-8")).digest()[:16]


def shiro1(name: str, password: str, algorithm: str, rounds: int) -> str:
    digest_name = SHIRO[algorithm]
    s = salt(name)
    h = hashlib.new(digest_name, s + password.encode("utf-8")).digest()
    for _ in range(rounds - 1):
        h = hashlib.new(digest_name, h).digest()
    return f"$shiro1${algorithm}${rounds}${b64(s)}${b64(h)}"


def pbkdf2(name: str, password: str, algorithm: str, rounds: int, bits: int) -> str:
    s = salt(name)
    h = hashlib.pbkdf2_hmac(PBKDF2[algorithm], password.encode("utf-8"), s, rounds, bits // 8)
    return f"${algorithm}$key_iteration={rounds},key_len={bits}${b64(s)}${b64(h)}"


def check_shiro1(password: str, stored: str) -> bool:
    _, _, algorithm, rounds, s, digest = stored.split("$")
    name = SHIRO[algorithm]
    h = hashlib.new(name, base64.b64decode(s) + password.encode("utf-8")).digest()
    for _ in range(int(rounds) - 1):
        h = hashlib.new(name, h).digest()
    return h == base64.b64decode(digest)


def main() -> None:
    public_shiro = ("$shiro1$SHA-512$1024$zjU1u+Zg9UNwuB+HEawvtA==$"
                    "IzF/OWzJxrqvB5FCe/2+UcZhhZYM2pTu0TEz7Ybnk65AbbEdUk9ntdtBzkN8P3gZby2qz6MHKqAe8Cjai9c4Gg==")
    public_shiro_wrong = public_shiro.replace("OWzJx", "OWzjx")
    assert check_shiro1("admin123", public_shiro)
    assert not check_shiro1("admin123", public_shiro_wrong)
    assert hashlib.sha1(b"admin123").hexdigest() == "f865b53623b121fd34ee5426c792e5c33af8c227"
    assert hashlib.md5(b"admin123").hexdigest() == "0192023a7bbd73250516f069df18b500"

    good = [
        ("public-shiro1-admin123", "admin123", public_shiro, PUBLIC),
        ("public-sha1-admin123", "admin123", "f865b53623b121fd34ee5426c792e5c33af8c227", PUBLIC),
        ("public-md5-admin123", "admin123", "0192023a7bbd73250516f069df18b500", PUBLIC),
    ]
    invented = [
        ("shiro1-sha512", "demo-admin-pass-1", lambda n, p: shiro1(n, p, "SHA-512", 1024)),
        ("shiro1-sha256", "demo-reader-pass-1", lambda n, p: shiro1(n, p, "SHA-256", 1024)),
        ("shiro1-sha384", "demo-sha384-pass-1", lambda n, p: shiro1(n, p, "SHA-384", 7)),
        ("shiro1-sha1-one-round", "demo-sha1-rounds-1", lambda n, p: shiro1(n, p, "SHA-1", 1)),
        ("shiro1-md5-two-rounds", "demo-md5-rounds-1", lambda n, p: shiro1(n, p, "MD5", 2)),
        ("shiro1-unicode", UNICODE, lambda n, p: shiro1(n, p, "SHA-512", 1024)),
        ("shiro1-nobody", "demo-nobody-pass-1", lambda n, p: shiro1(n, p, "SHA-512", 1024)),
        ("shiro1-off", "demo-off-pass-1", lambda n, p: shiro1(n, p, "SHA-512", 1024)),
        ("shiro1-locked", "demo-locked-pass-1", lambda n, p: shiro1(n, p, "SHA-512", 1024)),
        ("shiro1-change", "demo-change-pass-1", lambda n, p: shiro1(n, p, "SHA-512", 1024)),
        ("shiro1-anonymous", "demo-anonymous-pass-1", lambda n, p: shiro1(n, p, "SHA-512", 1024)),
        ("shiro1-1024-bytes", "a" * 1024, lambda n, p: shiro1(n, p, "SHA-512", 1024)),
        ("pbkdf2-sha1", "demo-pbkdf2-sha1-1", lambda n, p: pbkdf2(n, p, "PBKDF2WithHmacSHA1", 1024, 128)),
        ("pbkdf2-sha256", "demo-pbkdf2-pass-1", lambda n, p: pbkdf2(n, p, "PBKDF2WithHmacSHA256", 10000, 256)),
        ("pbkdf2-unicode", UNICODE, lambda n, p: pbkdf2(n, p, "PBKDF2WithHmacSHA256", 10000, 256)),
        ("legacy-sha1", "demo-legacy-sha1-1", lambda n, p: hashlib.sha1(p.encode("utf-8")).hexdigest()),
        ("legacy-md5", "demo-legacy-md5-1", lambda n, p: hashlib.md5(p.encode("utf-8")).hexdigest()),
        ("legacy-sha1-unicode", UNICODE, lambda n, p: hashlib.sha1(p.encode("utf-8")).hexdigest()),
    ]
    for name, password, make in invented:
        good.append((name, password, make(name, password), INVENTED))

    over = "a" * 1025
    refused = [
        ("public-shiro1-admin123-changed", "admin123", public_shiro_wrong, PUBLIC,
         "one character of the digest changed"),
        ("public-sha1-admin123-changed", "admin123", "f865b53623b121fd34ee5426c792e5c33af8c228", PUBLIC,
         "last hex digit changed"),
        ("public-md5-admin123-changed", "admin123", "0192023a7bbd73250516f069df18b501", PUBLIC,
         "last hex digit changed"),
        ("shiro1-1025-bytes", over, shiro1("shiro1-1025-bytes", over, "SHA-512", 1024), INVENTED,
         "password longer than 1024 bytes, refused without hashing"),
        ("shiro1-too-many-rounds", "demo-rounds-1", shiro1("shiro1-too-many-rounds", "demo-rounds-1", "MD5", 1)
         .replace("$MD5$1$", "$MD5$10000001$"), INVENTED, "more than 10,000,000 rounds, refused without hashing"),
        ("pbkdf2-too-many-rounds", "demo-rounds-1",
         pbkdf2("pbkdf2-too-many-rounds", "demo-rounds-1", "PBKDF2WithHmacSHA1", 1, 128)
         .replace("key_iteration=1,", "key_iteration=10000001,"), INVENTED,
         "more than 10,000,000 rounds, refused without hashing"),
    ]

    doc = {
        "about": "Password vectors of internal/auth, written by make_passwords.py. Do not edit by hand.",
        "verify": [{"name": n, "password": p, "hash": h, "source": s} for n, p, h, s in good],
        "refuse": [{"name": n, "password": p, "hash": h, "source": s, "why": w} for n, p, h, s, w in refused],
    }
    OUT.write_text(json.dumps(doc, indent=1, ensure_ascii=True, sort_keys=True) + "\n", encoding="utf-8")
    print(f"{OUT.name}: {len(good)} to verify, {len(refused)} to refuse")


if __name__ == "__main__":
    main()
