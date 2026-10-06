#!/usr/bin/env python3
"""Unit tests for check-no-secret-log.py (TS-INV-2 gate).

Run: python3 .github/scripts/test_check_no_secret_log.py
"""
import importlib.util
import pathlib
import tempfile
import unittest

_spec = importlib.util.spec_from_file_location(
    "gate", pathlib.Path(__file__).with_name("check-no-secret-log.py")
)
gate = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(gate)


def offenses(src: str) -> list[str]:
    with tempfile.TemporaryDirectory() as d:
        p = pathlib.Path(d) / "x.go"
        p.write_text("package x\n" + src)
        return gate.check_file(p)


class Flags(unittest.TestCase):
    def test_original_shapes_still_caught(self):
        self.assertTrue(offenses('log.Warn("rotated", map[string]any{\n\t"secret": v,\n})'))
        self.assertTrue(offenses('h.log.Info("issued", map[string]any{"id": res.Secret})'))
        self.assertTrue(offenses('slog.Info("x", "client_secret", v)'))

    def test_pem_variables_in_logs(self):
        self.assertTrue(offenses('log.Error("bad key", map[string]any{"v": pemStr})'))
        self.assertTrue(offenses('log.Debug("k", map[string]any{"k": privKey})'))
        self.assertTrue(offenses('log.Debug("k", map[string]any{"private_key": k})'))
        self.assertTrue(offenses('log.Debug("k", map[string]any{"key_material": k})'))

    def test_error_constructors(self):
        self.assertTrue(offenses('return fmt.Errorf("parse failed: %s", pemStr)'))
        self.assertTrue(offenses('return errors.New(string(priv))'))
        self.assertTrue(offenses('return fmt.Errorf("pem=%s", s)'))

    def test_span_attributes(self):
        self.assertTrue(offenses('span.SetAttributes(attribute.String("private_key", k))'))
        self.assertTrue(offenses('span.SetAttributes(attribute.String("k", pemStr))'))

    def test_pem_armor_literal(self):
        self.assertTrue(offenses('log.Info("-----BEGIN RSA PRIVATE KEY-----")'))
        self.assertTrue(offenses('return errors.New("-----BEGIN PUBLIC KEY-----")'))


class DoesNotFlag(unittest.TestCase):
    def test_prose_naming_the_concept(self):
        self.assertEqual([], offenses('return errors.New("service: OpenBao material is not PEM-encoded")'))
        self.assertEqual([], offenses('return fmt.Errorf("no secret at path")'))
        self.assertEqual([], offenses('return fmt.Errorf("decode private key: %w", err)'))

    def test_allowlisted_identifier(self):
        self.assertEqual([], offenses('return fmt.Errorf("secret data missing %q field", secretDataKey)'))

    def test_priv_word_boundaries(self):
        self.assertTrue(offenses('log.Info("x", map[string]any{"v": rsaPriv})'))
        self.assertEqual([], offenses('log.Info("x", map[string]any{"v": privilege})'))

    def test_unrelated_calls(self):
        self.assertEqual([], offenses('log.Info("issued", map[string]any{"kid": kid, "version": v})'))
        self.assertEqual([], offenses('x := pemStr; _ = x'))

    def test_paren_inside_string_does_not_end_the_call_early(self):
        self.assertTrue(offenses('log.Info("a ) b", map[string]any{"v": pemStr})'))


if __name__ == "__main__":
    unittest.main()
