#!/bin/bash

set -e

# ============================================================================
# rara-harvest Deployment Script
# Deploys Docker image to Artifact Registry and creates Cloud Run Job
# ============================================================================

# Configuration variables - EDIT THESE
PROJECT_ID="your-gcp-project-id"
REGION="us-central1"
REPO_NAME="rara"
JOB_NAME="rara-harvest"
IMAGE_NAME="rara-harvest"

# Derived variables
ARTIFACT_REGISTRY="${REGION}-docker.pkg.dev"
IMAGE_PATH="${ARTIFACT_REGISTRY}/${PROJECT_ID}/${REPO_NAME}/${IMAGE_NAME}:latest"

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Helper functions
log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."

    if ! command -v gcloud &> /dev/null; then
        log_error "gcloud CLI is not installed"
        exit 1
    fi

    if ! command -v docker &> /dev/null; then
        log_error "docker is not installed"
        exit 1
    fi

    log_info "Prerequisites OK"
}

# Authenticate with GCP
authenticate_gcp() {
    log_info "Setting GCP project to ${PROJECT_ID}..."
    gcloud config set project "${PROJECT_ID}"

    log_info "Authenticating Docker with Artifact Registry..."
    gcloud auth configure-docker "${ARTIFACT_REGISTRY}"
}

# Create Artifact Registry repository if it doesn't exist
create_artifact_registry() {
    log_info "Checking Artifact Registry repository..."

    if gcloud artifacts repositories describe "${REPO_NAME}" \
        --location="${REGION}" &>/dev/null; then
        log_info "Repository ${REPO_NAME} already exists"
    else
        log_info "Creating Artifact Registry repository..."
        gcloud artifacts repositories create "${REPO_NAME}" \
            --repository-format=docker \
            --location="${REGION}" \
            --description="Docker repository for YouTube ETL job"
    fi
}

# Build Docker image
build_docker_image() {
    log_info "Building Docker image for ARM64 architecture..."

    docker build \
        --platform linux/arm64 \
        -t "${IMAGE_NAME}:latest" \
        -t "${IMAGE_PATH}" \
        .

    if [ $? -eq 0 ]; then
        log_info "Docker image built successfully"
    else
        log_error "Failed to build Docker image"
        exit 1
    fi
}

# Push image to Artifact Registry
push_to_registry() {
    log_info "Pushing image to Artifact Registry..."

    docker push "${IMAGE_PATH}"

    if [ $? -eq 0 ]; then
        log_info "Image pushed successfully: ${IMAGE_PATH}"
    else
        log_error "Failed to push image"
        exit 1
    fi
}

# Create or update Cloud Run Job
deploy_cloud_run_job() {
    log_info "Deploying to Cloud Run..."

    # Check if job exists
    if gcloud run jobs describe "${JOB_NAME}" --region="${REGION}" &>/dev/null; then
        log_info "Updating existing Cloud Run job..."
        gcloud run jobs update "${JOB_NAME}" \
            --region="${REGION}" \
            --image="${IMAGE_PATH}" \
            --set-secrets="YOUTUBE_API_KEY=youtube-api-key:latest,DATABASE_URL=database-url:latest"
    else
        log_info "Creating new Cloud Run job..."
        gcloud run jobs create "${JOB_NAME}" \
            --image="${IMAGE_PATH}" \
            --region="${REGION}" \
            --set-secrets="YOUTUBE_API_KEY=youtube-api-key:latest,DATABASE_URL=database-url:latest" \
            --memory=512Mi \
            --cpu=1 \
            --task-timeout=1800s \
            --no-gen2
    fi

    if [ $? -eq 0 ]; then
        log_info "Cloud Run job deployed successfully"
    else
        log_error "Failed to deploy Cloud Run job"
        exit 1
    fi
}

# Create Cloud Scheduler trigger (optional)
setup_cloud_scheduler() {
    log_info "Setting up Cloud Scheduler trigger..."

    # Daily execution at 2 AM UTC
    SCHEDULE="0 2 * * *"
    SCHEDULER_NAME="${JOB_NAME}-scheduler"

    if gcloud scheduler jobs describe "${SCHEDULER_NAME}" --location="${REGION}" &>/dev/null; then
        log_info "Scheduler job already exists"
    else
        log_info "Creating Cloud Scheduler job..."
        gcloud scheduler jobs create http "${SCHEDULER_NAME}" \
            --schedule="${SCHEDULE}" \
            --location="${REGION}" \
            --uri="https://${REGION}-run.googleapis.com/apis/run.googleapis.com/v1/namespaces/${PROJECT_ID}/jobs/${JOB_NAME}:run" \
            --http-method=POST \
            --oidc-service-account-email="${PROJECT_ID}@appspot.gserviceaccount.com"
    fi
}

# Create secrets in Secret Manager (instructions)
setup_secrets() {
    log_info "Creating secrets in Secret Manager..."

    log_warn "MANUAL STEP REQUIRED:"
    log_warn "You must create two secrets in Secret Manager:"
    echo ""
    echo "1. YouTube API Key:"
    echo "   gcloud secrets create youtube-api-key --replication-policy=automatic --data-file=- <<< 'YOUR_YOUTUBE_API_KEY'"
    echo ""
    echo "2. Database URL:"
    echo "   gcloud secrets create database-url --replication-policy=automatic --data-file=- <<< 'postgresql://user:password@host:port/database'"
    echo ""
    log_warn "Grant Cloud Run service account access to these secrets:"
    echo "   gcloud secrets add-iam-policy-binding youtube-api-key --member=serviceAccount:${PROJECT_ID}@appspot.gserviceaccount.com --role=roles/secretmanager.secretAccessor"
    echo "   gcloud secrets add-iam-policy-binding database-url --member=serviceAccount:${PROJECT_ID}@appspot.gserviceaccount.com --role=roles/secretmanager.secretAccessor"
}

# Main deployment flow
main() {
    log_info "Starting YouTube ETL Job deployment..."
    log_info "Project: ${PROJECT_ID}"
    log_info "Region: ${REGION}"
    log_info "Image: ${IMAGE_PATH}"
    echo ""

    check_prerequisites
    authenticate_gcp
    create_artifact_registry
    build_docker_image
    push_to_registry
    deploy_cloud_run_job
    setup_cloud_scheduler
    setup_secrets

    echo ""
    log_info "Deployment completed successfully!"
    log_info "Job Name: ${JOB_NAME}"
    log_info "Region: ${REGION}"
    log_info "View job details:"
    echo "   gcloud run jobs describe ${JOB_NAME} --region=${REGION}"
    echo ""
    log_info "View job executions:"
    echo "   gcloud run jobs log ${JOB_NAME} --region=${REGION}"
    echo ""
}

main "$@"
