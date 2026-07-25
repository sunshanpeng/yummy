#!/usr/bin/env sh
set -eu

cert_dir=${1:-var/certs}
worker_id=${2:-worker-1}

case "$worker_id" in
  *[!A-Za-z0-9._-]* | "")
    echo "worker ID 只能包含字母、数字、点、下划线和连字符" >&2
    exit 1
    ;;
esac

mkdir -p "$cert_dir"

openssl req \
  -x509 \
  -newkey rsa:2048 \
  -nodes \
  -days 3650 \
  -keyout "$cert_dir/ca.key" \
  -out "$cert_dir/ca.crt" \
  -subj "/CN=Multi Topology Demo CA" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"

openssl req \
  -newkey rsa:2048 \
  -nodes \
  -keyout "$cert_dir/server.key" \
  -out "$cert_dir/server.csr" \
  -subj "/CN=localhost"

cat > "$cert_dir/server.ext" <<EOF
subjectAltName=DNS:localhost,IP:127.0.0.1
extendedKeyUsage=serverAuth
EOF

openssl x509 \
  -req \
  -days 825 \
  -in "$cert_dir/server.csr" \
  -CA "$cert_dir/ca.crt" \
  -CAkey "$cert_dir/ca.key" \
  -CAcreateserial \
  -extfile "$cert_dir/server.ext" \
  -out "$cert_dir/server.crt"

openssl req \
  -newkey rsa:2048 \
  -nodes \
  -keyout "$cert_dir/$worker_id.key" \
  -out "$cert_dir/$worker_id.csr" \
  -subj "/CN=$worker_id"

cat > "$cert_dir/$worker_id.ext" <<EOF
extendedKeyUsage=clientAuth
EOF

openssl x509 \
  -req \
  -days 825 \
  -in "$cert_dir/$worker_id.csr" \
  -CA "$cert_dir/ca.crt" \
  -CAkey "$cert_dir/ca.key" \
  -CAcreateserial \
  -extfile "$cert_dir/$worker_id.ext" \
  -out "$cert_dir/$worker_id.crt"

chmod 600 "$cert_dir"/*.key
rm "$cert_dir/server.csr" "$cert_dir/$worker_id.csr" "$cert_dir/server.ext" "$cert_dir/$worker_id.ext" "$cert_dir/ca.srl"

echo "开发证书已生成到 $cert_dir"
