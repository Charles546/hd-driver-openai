#!/bin/bash
set -e
if grep -q "load_secret_driver" .hdci.yml && grep -q "github_checkout" .hdci.yml; then
  echo "✅ Compliance check passed."
else
  echo "❌ Compliance error."
  exit 1
fi
