#!/bin/bash

# Monitor distill_ollama.py progress every 15 minutes
# Stops the process if no progress is detected

LOG_DIR="/Users/bardi/Projects/Github/rara"
MONITOR_INTERVAL=900  # 15 minutes in seconds
MAX_NO_PROGRESS_CHECKS=2  # Stop after 2 checks with no progress (30 min total)

last_line_count=0
no_progress_count=0

while true; do
    sleep $MONITOR_INTERVAL
    
    # Get the most recent log file
    latest_log=$(ls -t "$LOG_DIR"/run_ollama_*.log 2>/dev/null | head -1)
    
    if [ -z "$latest_log" ]; then
        echo "[$(date '+%Y-%m-%d %H:%M:%S')] ❌ No log file found!"
        continue
    fi
    
    # Count lines in log
    current_line_count=$(wc -l < "$latest_log")
    
    # Show current status
    echo ""
    echo "════════════════════════════════════════════════════════"
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 📊 Progress Check"
    echo "Log: $(basename $latest_log)"
    echo "Lines: $current_line_count (was $last_line_count)"
    
    # Show last 3 lines
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    tail -3 "$latest_log"
    echo "════════════════════════════════════════════════════════"
    
    # Check if progress was made
    if [ $current_line_count -eq $last_line_count ]; then
        ((no_progress_count++))
        echo "⚠️  No progress! Count: $no_progress_count/$MAX_NO_PROGRESS_CHECKS"
        
        if [ $no_progress_count -ge $MAX_NO_PROGRESS_CHECKS ]; then
            echo ""
            echo "❌ STOPPING - No progress detected after $((MAX_NO_PROGRESS_CHECKS * MONITOR_INTERVAL / 60)) minutes!"
            echo "Killing process..."
            pkill -f "distill_ollama.py"
            echo "✅ Process stopped."
            echo "Check the log for errors: $latest_log"
            break
        fi
    else
        no_progress_count=0
        progress=$((current_line_count - last_line_count))
        echo "✅ Progress detected! (+$progress lines)"
    fi
    
    last_line_count=$current_line_count
done
