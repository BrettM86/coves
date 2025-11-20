#!/bin/bash
# Generate cryptographic keys for Coves did:web DID document
#
# This script generates a secp256k1 (K-256) key pair as required by atproto.
# Reference: https://atproto.com/specs/cryptography
#
# Key format:
#   - Curve: secp256k1 (K-256) - same as Bitcoin/Ethereum
#   - Type: Multikey
#   - Encoding: publicKeyMultibase with base58btc ('z' prefix)
#   - Multicodec: 0xe7 for secp256k1 compressed public key
#
# Output:
#   - Private key (hex) for PDS_PLC_ROTATION_KEY_K256_PRIVATE_KEY_HEX
#   - Public key (multibase) for did.json publicKeyMultibase field
#   - Complete did.json file

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
OUTPUT_DIR="$PROJECT_DIR/static/.well-known"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log() { echo -e "${GREEN}[KEYGEN]${NC} $1"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
error() { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

# Check for required tools
if ! command -v openssl &> /dev/null; then
    error "openssl is required but not installed"
fi

if ! command -v python3 &> /dev/null; then
    error "python3 is required for base58 encoding"
fi

# Check for base58 library
if ! python3 -c "import base58" 2>/dev/null; then
    warn "Installing base58 Python library..."
    pip3 install base58 || error "Failed to install base58. Run: pip3 install base58"
fi

log "Generating secp256k1 key pair for did:web..."

# Generate private key
PRIVATE_KEY_PEM=$(mktemp)
openssl ecparam -name secp256k1 -genkey -noout -out "$PRIVATE_KEY_PEM" 2>/dev/null

# Extract private key as hex (for PDS config)
PRIVATE_KEY_HEX=$(openssl ec -in "$PRIVATE_KEY_PEM" -text -noout 2>/dev/null | \
    grep -A 3 "priv:" | tail -n 3 | tr -d ' :\n' | tr -d '\r')

# Extract public key as compressed format
# OpenSSL outputs the public key, we need to get the compressed form
PUBLIC_KEY_HEX=$(openssl ec -in "$PRIVATE_KEY_PEM" -pubout -conv_form compressed -outform DER 2>/dev/null | \
    tail -c 33 | xxd -p | tr -d '\n')

# Clean up temp file
rm -f "$PRIVATE_KEY_PEM"

# Encode public key as multibase with multicodec
# Multicodec 0xe7 = secp256k1 compressed public key
# Then base58btc encode with 'z' prefix
PUBLIC_KEY_MULTIBASE=$(python3 << EOF
import base58

# Compressed public key bytes
pub_hex = "$PUBLIC_KEY_HEX"
pub_bytes = bytes.fromhex(pub_hex)

# Prepend multicodec 0xe7 for secp256k1-pub
# 0xe7 as varint is just 0xe7 (single byte, < 128)
multicodec = bytes([0xe7, 0x01])  # 0xe701 for secp256k1-pub compressed
key_with_codec = multicodec + pub_bytes

# Base58btc encode
encoded = base58.b58encode(key_with_codec).decode('ascii')

# Add 'z' prefix for multibase
print('z' + encoded)
EOF
)

log "Keys generated successfully!"
echo ""
echo "============================================"
echo "  PRIVATE KEY (keep secret!)"
echo "============================================"
echo ""
echo "Add this to your .env.prod file:"
echo ""
echo "PDS_ROTATION_KEY=$PRIVATE_KEY_HEX"
echo ""
echo "============================================"
echo "  PUBLIC KEY (for did.json)"
echo "============================================"
echo ""
echo "publicKeyMultibase: $PUBLIC_KEY_MULTIBASE"
echo ""

# Generate the did.json file
log "Generating did.json..."

mkdir -p "$OUTPUT_DIR"

cat > "$OUTPUT_DIR/did.json" << EOF
{
  "id": "did:web:coves.social",
  "alsoKnownAs": ["at://coves.social"],
  "verificationMethod": [
    {
      "id": "did:web:coves.social#atproto",
      "type": "Multikey",
      "controller": "did:web:coves.social",
      "publicKeyMultibase": "$PUBLIC_KEY_MULTIBASE"
    }
  ],
  "service": [
    {
      "id": "#atproto_pds",
      "type": "AtprotoPersonalDataServer",
      "serviceEndpoint": "https://coves.me"
    }
  ]
}
EOF

log "Created: $OUTPUT_DIR/did.json"
echo ""
echo "============================================"
echo "  NEXT STEPS"
echo "============================================"
echo ""
echo "1. Copy the PDS_ROTATION_KEY value to your .env.prod file"
echo ""
echo "2. Verify the did.json looks correct:"
echo "   cat $OUTPUT_DIR/did.json"
echo ""
echo "3. After deployment, verify it's accessible:"
echo "   curl https://coves.social/.well-known/did.json"
echo ""
warn "IMPORTANT: Keep the private key secret! Only share the public key."
warn "The did.json file with the public key IS safe to commit to git."
