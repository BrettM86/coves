#!/bin/bash
# Derive public key from existing PDS_ROTATION_KEY and create did.json
#
# This script takes your existing private key and derives the public key from it.
# Use this if you already have a PDS running with a rotation key but need to
# create/fix the did.json file.
#
# Usage: ./scripts/derive-did-from-key.sh

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
OUTPUT_DIR="$PROJECT_DIR/static/.well-known"

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log() { echo -e "${GREEN}[DERIVE]${NC} $1"; }
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

# Load environment to get the existing key
if [ -f "$PROJECT_DIR/.env.prod" ]; then
    source "$PROJECT_DIR/.env.prod"
elif [ -f "$PROJECT_DIR/.env" ]; then
    source "$PROJECT_DIR/.env"
else
    error "No .env.prod or .env file found"
fi

if [ -z "$PDS_ROTATION_KEY" ]; then
    error "PDS_ROTATION_KEY not found in environment"
fi

# Validate key format (should be 64 hex chars)
if [[ ! "$PDS_ROTATION_KEY" =~ ^[0-9a-fA-F]{64}$ ]]; then
    error "PDS_ROTATION_KEY is not a valid 64-character hex string"
fi

log "Deriving public key from existing PDS_ROTATION_KEY..."

# Create a temporary PEM file from the hex private key
TEMP_DIR=$(mktemp -d)
PRIVATE_KEY_HEX="$PDS_ROTATION_KEY"

# Convert hex private key to PEM format
# secp256k1 curve OID: 1.3.132.0.10
python3 > "$TEMP_DIR/private.pem" << EOF
import binascii

# Private key in hex
priv_hex = "$PRIVATE_KEY_HEX"
priv_bytes = binascii.unhexlify(priv_hex)

# secp256k1 OID
oid = bytes([0x06, 0x05, 0x2b, 0x81, 0x04, 0x00, 0x0a])

# Build the EC private key structure
# SEQUENCE { version INTEGER, privateKey OCTET STRING, [0] OID, [1] publicKey }
# We'll use a simpler approach: just the private key with curve params

# EC PARAMETERS for secp256k1
ec_params = bytes([
    0x30, 0x07,  # SEQUENCE, 7 bytes
    0x06, 0x05, 0x2b, 0x81, 0x04, 0x00, 0x0a  # OID for secp256k1
])

# EC PRIVATE KEY structure
# SEQUENCE { version, privateKey, [0] parameters }
inner = bytes([0x02, 0x01, 0x01])  # version = 1
inner += bytes([0x04, 0x20]) + priv_bytes  # OCTET STRING with 32-byte key
inner += bytes([0xa0, 0x07]) + bytes([0x06, 0x05, 0x2b, 0x81, 0x04, 0x00, 0x0a])  # [0] OID

# Wrap in SEQUENCE
key_der = bytes([0x30, len(inner)]) + inner

# Base64 encode
import base64
key_b64 = base64.b64encode(key_der).decode('ascii')

# Format as PEM
print("-----BEGIN EC PRIVATE KEY-----")
for i in range(0, len(key_b64), 64):
    print(key_b64[i:i+64])
print("-----END EC PRIVATE KEY-----")
EOF

# Extract the compressed public key
PUBLIC_KEY_HEX=$(openssl ec -in "$TEMP_DIR/private.pem" -pubout -conv_form compressed -outform DER 2>/dev/null | \
    tail -c 33 | xxd -p | tr -d '\n')

# Clean up
rm -rf "$TEMP_DIR"

if [ -z "$PUBLIC_KEY_HEX" ] || [ ${#PUBLIC_KEY_HEX} -ne 66 ]; then
    error "Failed to derive public key. Got: $PUBLIC_KEY_HEX"
fi

log "Derived public key: ${PUBLIC_KEY_HEX:0:8}...${PUBLIC_KEY_HEX: -8}"

# Encode public key as multibase with multicodec
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

log "Public key multibase: $PUBLIC_KEY_MULTIBASE"

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
echo "  DID Document Generated Successfully!"
echo "============================================"
echo ""
echo "Public key multibase: $PUBLIC_KEY_MULTIBASE"
echo ""
echo "Next steps:"
echo "  1. Copy this file to your production server:"
echo "     scp $OUTPUT_DIR/did.json user@server:/opt/coves/static/.well-known/"
echo ""
echo "  2. Or if running on production, restart Caddy:"
echo "     docker compose -f docker-compose.prod.yml restart caddy"
echo ""
echo "  3. Verify it's accessible:"
echo "     curl https://coves.social/.well-known/did.json"
echo ""
