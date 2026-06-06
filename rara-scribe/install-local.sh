#!/usr/bin/env bash
# install-local.sh — instala o rara-scribe como agente launchd no Mac.
# Corre uma vez para configurar; re-corre após `make build` para actualizar o binário.
#
# Uso: bash install-local.sh (a partir da directoria rara-scribe/)
set -euo pipefail

INSTALL_DIR="$HOME/.rara-scribe"
PLIST_PATH="$HOME/Library/LaunchAgents/com.rara.scribe.plist"
LOG_DIR="$HOME/Library/Logs/rara-scribe"
LABEL="com.rara.scribe"

# ---------------------------------------------------------------------------
# 0. Guarda de directório — deve correr a partir de rara-scribe/
# ---------------------------------------------------------------------------
if [ ! -f "go.mod" ] || [ ! -f ".env.example" ]; then
    echo "!! Corre este script a partir da directoria rara-scribe/:"
    echo "   cd rara-scribe && bash install-local.sh"
    echo "   (ou: make install-local)"
    exit 1
fi

# ---------------------------------------------------------------------------
# 1. Pré-requisitos
# ---------------------------------------------------------------------------
echo "==> Verificando pré-requisitos..."
missing=()
for cmd in go yt-dlp ffmpeg; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
        missing+=("$cmd")
    fi
done
if [ ${#missing[@]} -gt 0 ]; then
    echo ""
    echo "!! Faltam os seguintes comandos: ${missing[*]}"
    echo "   Instala via Homebrew:"
    echo "     brew install go yt-dlp ffmpeg"
    exit 1
fi
echo "   go: $(command -v go)"
echo "   yt-dlp: $(command -v yt-dlp)"
echo "   ffmpeg: $(command -v ffmpeg)"

# ---------------------------------------------------------------------------
# 2. Criar directórios
# ---------------------------------------------------------------------------
mkdir -p "$INSTALL_DIR" "$LOG_DIR"

# ---------------------------------------------------------------------------
# 3. Criar .env se não existir (termina aqui na primeira execução)
# ---------------------------------------------------------------------------
if [ ! -f "$INSTALL_DIR/.env" ]; then
    echo ""
    echo "==> Criando $INSTALL_DIR/.env a partir do template..."
    YT_DLP_REAL=$(command -v yt-dlp)
    FFMPEG_REAL=$(command -v ffmpeg)
    sed \
        -e "s|YT_DLP_BIN=.*|YT_DLP_BIN=$YT_DLP_REAL|" \
        -e "s|FFMPEG_BIN=.*|FFMPEG_BIN=$FFMPEG_REAL|" \
        .env.example > "$INSTALL_DIR/.env"
    echo ""
    echo "!! PASSO OBRIGATÓRIO: edita o ficheiro antes de continuar:"
    echo "   \$EDITOR $INSTALL_DIR/.env"
    echo ""
    echo "   Preenche DATABASE_URL e GROQ_API_KEY com os valores reais."
    echo "   Os caminhos de yt-dlp e ffmpeg já foram detectados automaticamente."
    echo ""
    echo "   Depois corre de novo: bash install-local.sh"
    exit 0
fi

# ---------------------------------------------------------------------------
# 4. Compilar o binário
# ---------------------------------------------------------------------------
echo ""
echo "==> Compilando rara-scribe..."
go build -ldflags="-w -s" -o "$INSTALL_DIR/rara-scribe" .
echo "   Binário: $INSTALL_DIR/rara-scribe"

# ---------------------------------------------------------------------------
# 5. Wrapper script chamado pelo launchd
# ---------------------------------------------------------------------------
cat > "$INSTALL_DIR/run.sh" << WRAPPER
#!/bin/bash
# Wrapper gerado por install-local.sh — não editar directamente.
set -a
source "$INSTALL_DIR/.env"
set +a
exec "$INSTALL_DIR/rara-scribe"
WRAPPER
chmod +x "$INSTALL_DIR/run.sh"

# ---------------------------------------------------------------------------
# 6. Gerar e instalar o plist launchd
# ---------------------------------------------------------------------------
mkdir -p "$(dirname "$PLIST_PATH")"
cat > "$PLIST_PATH" << PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>

    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>$INSTALL_DIR/run.sh</string>
    </array>

    <!-- Corre diariamente às 02:00. Para alterar, edita este plist e
         re-executa: launchctl unload $PLIST_PATH && launchctl load $PLIST_PATH -->
    <key>StartCalendarInterval</key>
    <dict>
        <key>Hour</key>
        <integer>2</integer>
        <key>Minute</key>
        <integer>0</integer>
    </dict>

    <key>StandardOutPath</key>
    <string>$LOG_DIR/output.log</string>
    <key>StandardErrorPath</key>
    <string>$LOG_DIR/error.log</string>

    <!-- Não corre imediatamente ao carregar, só no próximo horário agendado -->
    <key>RunAtLoad</key>
    <false/>
</dict>
</plist>
PLIST

# ---------------------------------------------------------------------------
# 7. Carregar (ou recarregar) o agente launchd
# ---------------------------------------------------------------------------
# Descarregar silenciosamente se já estava carregado (atualização)
launchctl unload "$PLIST_PATH" 2>/dev/null || true
launchctl load "$PLIST_PATH"

# ---------------------------------------------------------------------------
# 8. Instruções finais
# ---------------------------------------------------------------------------
echo ""
echo "✅ rara-scribe instalado e agendado (diariamente às 02:00)."
echo ""
echo "   Config:   $INSTALL_DIR/.env"
echo "   Binário:  $INSTALL_DIR/rara-scribe"
echo "   Logs:     $LOG_DIR/"
echo "   Plist:    $PLIST_PATH"
echo ""
echo "Comandos úteis:"
echo "  Forçar run agora:    launchctl start $LABEL"
echo "  Ver logs (live):     tail -f $LOG_DIR/output.log"
echo "  Ver erros:           tail -f $LOG_DIR/error.log"
echo "  Parar serviço:       launchctl unload $PLIST_PATH"
echo "  Actualizar binário:  cd rara-scribe && make build && bash install-local.sh"
echo ""
echo "Validar no Neon após o primeiro run:"
echo "  SELECT status, COUNT(*) FROM transcripts GROUP BY status;"
