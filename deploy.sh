#!/bin/bash

# kuma_bot Lambda デプロイスクリプト

set -e

# デフォルト値の設定
PROFILE=${1:-lambda-deploy}
FUNCTION_NAME=${2:-kuma}

# AWS CLIのページャーを無効化
export AWS_PAGER=""

echo "Building kuma_bot for Lambda..."
GOOS=linux GOARCH=amd64 go build -o bootstrap main.go

echo "Creating deployment package..."
zip kuma_bot.zip bootstrap

echo "Deploying to Lambda..."
echo "Profile: $PROFILE"
echo "Function Name: $FUNCTION_NAME"
aws lambda update-function-code \
    --function-name "$FUNCTION_NAME" \
    --zip-file fileb://kuma_bot.zip \
    --region ap-northeast-1 \
    --profile "$PROFILE"

echo "Cleaning up..."
rm bootstrap kuma_bot.zip

echo "Deployment completed successfully!"