#!/usr/bin/env bash

# wait-for-job.sh - Wait for a Kubernetes job to complete while streaming logs
# Usage: wait-for-job.sh <job-name> <namespace> [timeout-seconds]

set -euo pipefail

JOB_NAME="${1:-}"
NAMESPACE="${2:-}"
TIMEOUT="${3:-600}"

if [ -z "$JOB_NAME" ] || [ -z "$NAMESPACE" ]; then
    echo "Usage: $0 <job-name> <namespace> [timeout-seconds]"
    echo "Example: $0 cluster-proxy-e2e open-cluster-management-addon 600"
    exit 1
fi

INTERVAL=5
MAX_ITERATIONS=$((TIMEOUT / INTERVAL))

echo "Monitoring job '$JOB_NAME' in namespace '$NAMESPACE'..."
echo "Tip: You can also watch logs in another terminal with:"
echo "  kubectl logs -f --tail=100 job/$JOB_NAME -n $NAMESPACE"
echo ""

# Start streaming logs in background
kubectl logs -f "job/$JOB_NAME" -n "$NAMESPACE" 2>/dev/null &
LOG_PID=$!

# Function to clean up background process
cleanup() {
    if kill -0 $LOG_PID 2>/dev/null; then
        kill $LOG_PID 2>/dev/null || true
        wait $LOG_PID 2>/dev/null || true
    fi
}

# Trap to ensure cleanup on exit
trap cleanup EXIT

# Poll job status
for i in $(seq 1 "$MAX_ITERATIONS"); do
    # Get job status conditions
    COMPLETE=$(kubectl get "job/$JOB_NAME" -n "$NAMESPACE" \
        -o jsonpath='{.status.conditions[?(@.type=="Complete")].status}' 2>/dev/null || echo "")
    FAILED=$(kubectl get "job/$JOB_NAME" -n "$NAMESPACE" \
        -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || echo "")

    if [ "$COMPLETE" = "True" ]; then
        cleanup
        echo ""
        echo "✓ Job '$JOB_NAME' completed successfully"
        exit 0
    elif [ "$FAILED" = "True" ]; then
        cleanup
        echo ""
        echo "✗ Job '$JOB_NAME' failed"

        # Try to show failure reason
        REASON=$(kubectl get "job/$JOB_NAME" -n "$NAMESPACE" \
            -o jsonpath='{.status.conditions[?(@.type=="Failed")].reason}' 2>/dev/null || echo "")
        MESSAGE=$(kubectl get "job/$JOB_NAME" -n "$NAMESPACE" \
            -o jsonpath='{.status.conditions[?(@.type=="Failed")].message}' 2>/dev/null || echo "")

        if [ -n "$REASON" ]; then
            echo "Reason: $REASON"
        fi
        if [ -n "$MESSAGE" ]; then
            echo "Message: $MESSAGE"
        fi

        # Print job logs
        echo ""
        echo "=== Job Logs ==="
        kubectl logs "job/$JOB_NAME" -n "$NAMESPACE" || echo "Failed to retrieve job logs"
        echo "=== End of Job Logs ==="
        echo ""

        # Print pod information for additional debugging
        echo "=== Pod Status ==="
        kubectl get pods -n "$NAMESPACE" -l "job-name=$JOB_NAME" -o wide || echo "Failed to retrieve pod information"
        echo ""

        exit 1
    fi

    # Show progress every 30 seconds
    if [ $((i % 6)) -eq 0 ]; then
        ELAPSED=$((i * INTERVAL))
        echo "[$(date '+%H:%M:%S')] Still waiting... (${ELAPSED}s elapsed)"
    fi

    sleep "$INTERVAL"
done

# Timeout reached
cleanup
echo ""
echo "✗ Job '$JOB_NAME' timed out after ${TIMEOUT}s"
exit 1
