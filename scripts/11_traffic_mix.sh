#!/bin/bash
# 11_traffic_mix.sh
#
#
# Descripción:
#   Simula peticiones de usuarios reales enviando peticiones variadas (lecturas HTTP/HTTPS,
#   búsquedas, accesos a APIs REST, envíos de formularios por POST y consultas SQL a la base de datos)
#   para validar el comportamiento en carga de la infraestructura (Nginx, Apache, MariaDB y almacenamiento).
#
# Modos de ejecución:
#   - Modo Externo (--external): Simula peticiones externas apuntando a la IP pública (WAN) del router.
#   - Modo Interno (--internal): Simula peticiones directas desde la red interna (hot-desks) al balanceador.

set -euo pipefail

# Cargar la configuración global
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/config.sh"

# ─── Configuración por Defecto y Parámetros del Script ────────────────────────
ROUTER_WAN_IP=""                 # IP externa del router (autodetectada o especificada)
CMS_DOMAIN="cms.fake-enterprise.com"
DURATION=${DURATION:-60}         # Duración del test en segundos
CONCURRENCY=${CONCURRENCY:-5}    # Número de hilos concurrentes para Apache Bench
MODE=""                          # Modo seleccionado (internal / external)
VERBOSE=false
DB_HOST="${DB_HOST:-${MASTER1_IP}}" # Dirección del NodePort de base de datos
DB_PORT="${DB_PORT:-30306}"
PROTOCOL="https"                 # Protocolo por defecto

# Muestra el panel de ayuda y comandos de ejemplo
show_help() {
  cat <<'HELP'
Uso: 11_traffic_mix.sh [OPCIONES]

Simula patrones de navegación reales y carga sobre la infraestructura CMS.

OPCIONES:
  --external          Tráfico simulado desde el exterior. Requiere --target (IP WAN del router).
  --internal          Tráfico simulado desde un puesto de trabajo interno (apunta a main-lb).
  --target IP         Fuerza la dirección IP de destino.
  --duration SEG      Establece la duración de la prueba en segundos (Default: 60).
  --concurrency N     Ajusta el número de hilos de Apache Bench (Default: 5).
  --with-db           Ejecuta pruebas adicionales de consultas directas SQL.
  --verbose           Modo detallado: Muestra cada petición en consola.
  --help              Muestra este menú.

EJEMPLOS:
  # Ejecución interna desde un hotdesk por 2 minutos
  ./11_traffic_mix.sh --internal --duration 120

  # Simulación externa con queries a la base de datos
  ./11_traffic_mix.sh --external --target 10.0.0.1 --with-db
HELP
  exit 0
}

# ─── Parseo de Argumentos de Entrada ──────────────────────────────────────────
WITH_DB=false
TARGET_IP=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --external)    MODE="external"; shift ;;
    --internal)    MODE="internal"; shift ;;
    --target)      TARGET_IP="$2"; shift 2 ;;
    --duration)    DURATION="$2"; shift 2 ;;
    --concurrency) CONCURRENCY="$2"; shift 2 ;;
    --with-db)     WITH_DB=true; shift ;;
    --verbose)     VERBOSE=true; shift ;;
    --help|-h)     show_help ;;
    *)             error "Parámetro inválido: $1"; show_help ;;
  esac
done

if [[ -z "$MODE" ]]; then
  error "Debe seleccionar un modo obligatorio: --external o --internal"
  exit 1
fi

# ─── Resolución de la IP del Servidor de Destino ──────────────────────────────
if [[ -n "$TARGET_IP" ]]; then
  DEST_IP="$TARGET_IP"
elif [[ "$MODE" == "internal" ]]; then
  DEST_IP="$LB_IP"
elif [[ "$MODE" == "external" ]]; then
  if [[ -z "$ROUTER_WAN_IP" ]]; then
    error "Para ejecutar --external, debe indicar la IP WAN del router con --target <IP>"
    exit 1
  fi
  DEST_IP="$ROUTER_WAN_IP"
fi

BASE_URL="${PROTOCOL}://${DEST_IP}"

# ─── Validación de Dependencias Locales ───────────────────────────────────────
check_tool() {
  if ! command -v "$1" &>/dev/null; then
    warn "Comando '$1' ausente. Se omitirán las fases asociadas."
    return 1
  fi
  return 0
}

HAS_CURL=false; check_tool curl && HAS_CURL=true
HAS_AB=false; check_tool ab && HAS_AB=true
HAS_MYSQL=false; check_tool mysql && HAS_MYSQL=true

if ! $HAS_CURL; then
  error "curl es obligatorio para la ejecución. Instale: apt-get install curl"
  exit 1
fi

# ─── Contadores de Resultados y Estadísticas ──────────────────────────────────
TOTAL_REQUESTS=0
SUCCESS_REQUESTS=0
FAILED_REQUESTS=0
START_TIME=$(date +%s)

# Incrementa contadores en base a los códigos HTTP devueltos
record_result() {
  local http_code="$1"
  local url="$2"
  ((TOTAL_REQUESTS++)) || true
  # HTTP 405 Method Not Allowed es la respuesta correcta y esperada para peticiones GET a xmlrpc.php
  if [[ "$http_code" -ge 200 && "$http_code" -lt 400 ]] || [[ "$http_code" -eq 405 && "$url" == *"/xmlrpc.php"* ]]; then
    ((SUCCESS_REQUESTS++)) || true
    if $VERBOSE; then success "HTTP $http_code ← $url"; fi
  else
    ((FAILED_REQUESTS++)) || true
    if $VERBOSE; then warn "HTTP $http_code ← $url"; fi
  fi
}

# Invoca la petición curl y extrae el http_code final
do_request() {
  local url="$1"
  local http_code
  http_code=$(curl -sk -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 10 "$url" 2>/dev/null || echo "000")
  record_result "$http_code" "$url"
}

# Imprimir banner informativo
echo "========================================================="
echo "  TrafficMix — Generador de Tráfico CMS (Fake Enterprise)"
echo "========================================================="
info "Modo seleccionado:  $MODE"
info "IP objetivo:        $DEST_IP"
info "URL base:           $BASE_URL"
info "Duración:           ${DURATION}s"
info "Concurrencia (ab):  $CONCURRENCY"
info "Prueba base datos:  $WITH_DB"
echo "---------------------------------------------------------"

# ─── FASE 1: Peticiones HTTP Variadas (Lecturas Generales con Curl) ───────────
info "Fase 1: Generando lecturas y búsquedas distribuidas (curl)..."

# Rutas de WordPress típicas para simular visitas a posts, búsquedas y uso de APIs
WP_PATHS=(
  "/"                                  # Página de inicio
  "/wp-login.php"                      # Formulario de autenticación
  "/?s=test"                           # Búsquedas sencillas (Genera carga SQL)
  "/?s=empresa"
  "/?s=proyecto"
  "/?p=1"                              # Lectura de posts
  "/wp-admin/"                         # Acceso a administración
  "/wp-cron.php"                       # Cron interno de WordPress
  "/xmlrpc.php"                        # Endpoint XML-RPC
  "/wp-json/wp/v2/posts"               # API REST - Lectura de posts
  "/wp-json/wp/v2/pages"               # API REST - Lectura de páginas
  "/wp-json/wp/v2/users"               # API REST - Lectura de usuarios
  "/feed/"                             # RSS
  "/wp-content/themes/"
  "/favicon.ico"
  "/?cat=1"                            # Consultas por categoría
  "/?author=1"                         # Consultas por autor
  "/?m=$(date +%Y%m)"                         # Archivo histórico
)

END_TIME=$((START_TIME + DURATION))

info "Enviando consultas distribuidas por $DURATION segundos..."
while [[ $(date +%s) -lt $END_TIME ]]; do
  # Seleccionar una ruta aleatoria del pool
  idx=$((RANDOM % ${#WP_PATHS[@]}))
  path="${WP_PATHS[$idx]}"
  do_request "${BASE_URL}${path}"

  # Retardo aleatorio corto (50ms - 300ms) para emular comportamiento humano
  sleep "0.$(printf '%03d' $((RANDOM % 250 + 50)))"

  if (( TOTAL_REQUESTS % 10 == 0 )); then
    elapsed=$(( $(date +%s) - START_TIME ))
    remaining=$(( DURATION - elapsed ))
    info "Progreso: ${TOTAL_REQUESTS} peticiones | ${SUCCESS_REQUESTS} OK | ${FAILED_REQUESTS} fallidas | Tiempo restante: ${remaining}s"
  fi
done

success "Fase 1 completada: ${TOTAL_REQUESTS} peticiones curl enviadas."

# ─── FASE 2: Pruebas de Carga Concurrente (Apache Bench - ab) ──────────────────
if $HAS_AB; then
  echo ""
  info "Fase 2: Ejecutando pruebas de concurrencia y estrés con Apache Bench (ab)..."

  AB_REQUESTS=500
  AB_URL="${BASE_URL}/"

  info "Invocando: ab -n ${AB_REQUESTS} -c ${CONCURRENCY} -s 10 ${AB_URL}"

  # -k: Mantiene conexiones TCP persistentes (keepalive)
  # -s: Límite de timeout por socket
  ab_output=$(ab -n "$AB_REQUESTS" -c "$CONCURRENCY" -s 10 -k "$AB_URL" 2>&1 || true)

  # Filtrar métricas de rendimiento útiles
  rps=$(echo "$ab_output" | grep "Requests per second" | awk '{print $4}' || echo "N/A")
  time_per=$(echo "$ab_output" | grep "Time per request" | head -1 | awk '{print $4}' || echo "N/A")
  failed=$(echo "$ab_output" | grep "Failed requests" | awk '{print $3}' || echo "N/A")

  success "Resultados de Apache Bench (ab):"
  info "  Tasa de peticiones:    ${rps} peticiones/segundo"
  info "  Latencia media:        ${time_per} ms"
  info "  Peticiones fallidas:   ${failed}"
else
  echo ""
  warn "Fase 2 omitida: 'ab' no instalado. Instale: apt-get install apache2-utils"
fi

# ─── FASE 3: Consultas de Lectura Directa en la Base de Datos ──────────────────
if $WITH_DB && $HAS_MYSQL; then
  echo ""
  info "Fase 3: Ejecutando consultas y lecturas directas en MariaDB (NodePort)..."

  MYSQL_CMD="mysql -h ${DB_HOST} -P ${DB_PORT} -u ${DB_USER} -p${DB_PASS} ${DB_NAME}"

  DB_QUERIES=(
    "SELECT COUNT(*) FROM wp_posts;"
    "SELECT COUNT(*) FROM wp_users;"
    "SELECT COUNT(*) FROM wp_comments;"
    "SELECT * FROM wp_options WHERE option_name = 'blogname';"
    "SELECT * FROM wp_options WHERE option_name = 'siteurl';"
    "SELECT ID, post_title FROM wp_posts ORDER BY post_date DESC LIMIT 10;"
    "SELECT user_login, user_email FROM wp_users LIMIT 5;"
    "SELECT option_name, option_value FROM wp_options LIMIT 20;"
    "SHOW TABLES;"
    "SHOW TABLE STATUS;"
  )

  db_success=0
  db_fail=0

  for query in "${DB_QUERIES[@]}"; do
    if $MYSQL_CMD -e "$query" &>/dev/null; then
      ((db_success++)) || true
      if $VERBOSE; then success "SQL OK: $query"; fi
    else
      ((db_fail++)) || true
      if $VERBOSE; then warn "SQL FAIL: $query"; fi
    fi
  done

  success "Fase 3 completada: ${db_success} consultas correctas, ${db_fail} fallidas."
elif $WITH_DB && ! $HAS_MYSQL; then
  echo ""
  warn "Fase 3 omitida: Cliente 'mysql' ausente. Instale: apt-get install mysql-client"
else
  echo ""
  info "Fase 3 omitida: No se habilitó --with-db"
fi

# ─── FASE 4: Simulación de Envío de Formularios (POST) ────────────────────────
echo ""
info "Fase 4: Simulando escrituras y envío de datos (POST)..."

post_ok=0
post_fail=0

# Intentar logins erróneos para forzar escrituras de auditoría en base de datos
for i in $(seq 1 5); do
  http_code=$(curl -sk -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 10 \
    -X POST "${BASE_URL}/wp-login.php" \
    -d "log=admin&pwd=test_password_${i}&wp-submit=Acceder&redirect_to=%2Fwp-admin%2F&testcookie=1" \
    2>/dev/null || echo "000")

  if [[ "$http_code" -ge 200 && "$http_code" -lt 500 ]]; then
    ((post_ok++)) || true
  else
    ((post_fail++)) || true
  fi
done

# Simulación de búsquedas dinámicas por POST
SEARCH_TERMS=("WordPress" "CMS" "empresa" "administración" "contenido" "gestión" "red" "base de datos")
for term in "${SEARCH_TERMS[@]}"; do
  http_code=$(curl -sk -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 10 \
    -X POST "${BASE_URL}/" \
    -d "s=${term}" \
    2>/dev/null || echo "000")

  if [[ "$http_code" -ge 200 && "$http_code" -lt 400 ]]; then
    ((post_ok++)) || true
  else
    ((post_fail++)) || true
  fi
done

success "Fase 4 completada: ${post_ok} consultas POST exitosas, ${post_fail} fallidas."

# ─── Resumen Final ────────────────────────────────────────────────────────────
FINAL_TIME=$(date +%s)
TOTAL_TIME=$((FINAL_TIME - START_TIME))

echo ""
echo "========================================================="
echo "  RESUMEN DE RESULTADOS — TrafficMix"
echo "========================================================="
echo ""
info "Modo simulado:          $MODE"
info "IP objetivo:            $DEST_IP"
info "Tiempo real de test:    ${TOTAL_TIME}s"
echo ""
echo "  ┌─────────────────────────────────────────────────┐"
# Formateo de salida para alineación de columnas de estadísticas
echo "  │  Peticiones HTTP totales:  $(printf '%6d' $TOTAL_REQUESTS)               │"
echo "  │  Exitosas (2xx/3xx):      $(printf '%6d' $SUCCESS_REQUESTS)               │"
echo "  │  Fallidas:                $(printf '%6d' $FAILED_REQUESTS)               │"
if $HAS_AB; then
echo "  │  Rendimiento (ab):        ${rps:-N/A} req/s          │"
fi
echo "  └─────────────────────────────────────────────────┘"
echo ""

if (( FAILED_REQUESTS == 0 )); then
  success "✔ Conexión estable: El 100% de las consultas se procesaron sin errores."
elif (( FAILED_REQUESTS < TOTAL_REQUESTS / 10 )); then
  warn "⚠ Latencias altas: Ocurrió un bajo ratio de fallos (<10%). Revise recursos de VMs."
else
  error "✗ Clúster inestable: Alto ratio de errores HTTP. Verifique el estado de los servicios."
fi
echo "========================================================="
