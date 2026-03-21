#!/bin/bash
set -e

# 切換到腳本所在目錄
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"
echo "📁 工作目錄: $SCRIPT_DIR"

# ----------------------------------------
# 部署腳本配置
# ----------------------------------------
AWS_REGION="ap-northeast-1"
AWS_ACCOUNT_ID="483997780326"
ECR_REPOSITORY="mirror-service"
ECS_CLUSTER="membercenter-cluster"
ECS_SERVICE="mirror-service"

# 網路 & 安全組配置（與其他服務同 subnet）
PRIVATE_SUBNET_1="subnet-09c5690963a9d3223"
SECURITY_GROUP="sg-04573257fb4272561"

# CloudMap 配置（首次部署時需在 AWS Console 或 CLI 建立並填入）
CLOUDMAP_SVC_ID="${CLOUDMAP_MIRROR_SVC_ID:-}"

if [ -n "$CLOUDMAP_SVC_ID" ]; then
  CLOUDMAP_SVC_ARN=$(aws servicediscovery get-service \
                       --id "${CLOUDMAP_SVC_ID}" \
                       --region "${AWS_REGION}" \
                       --query 'Service.Arn' --output text)
  echo "💡 CloudMap Service ARN = ${CLOUDMAP_SVC_ARN}"
fi

# EFS 配置
EFS_FILE_SYSTEM_ID="${EFS_FILE_SYSTEM_ID:-fs-04575e906b767bc9a}"
EFS_ACCESS_POINT_ID="${EFS_ACCESS_POINT_ID:-fsap-05e878070ac812170}"

# 檢查參數
if [ -n "$1" ]; then
  AWS_ACCOUNT_ID="$1"
  echo "使用提供的 AWS 帳號 ID: $AWS_ACCOUNT_ID"
fi

# ----------------------------------------
# 前置檢查
# ----------------------------------------
if [ -z "$AWS_ACCOUNT_ID" ] || [ "$AWS_ACCOUNT_ID" = "XXXXXXXXXXXX" ]; then
  echo "錯誤: 請提供有效的 AWS_ACCOUNT_ID (可以作為第一個參數傳入)"; exit 1
fi

if [ -z "$EFS_FILE_SYSTEM_ID" ]; then
  echo "錯誤: EFS_FILE_SYSTEM_ID 未設定"; exit 1
fi
if [ -z "$EFS_ACCESS_POINT_ID" ]; then
  echo "錯誤: EFS_ACCESS_POINT_ID 未設定"; exit 1
fi

if ! command -v aws &>/dev/null; then
  echo "錯誤: AWS CLI 未安裝"; exit 1
fi

if ! command -v docker &>/dev/null; then
  echo "錯誤: Docker 未安裝"; exit 1
fi

if ! docker info &>/dev/null; then
  echo "錯誤: Docker daemon 未運行"; exit 1
fi

# ----------------------------------------
# 檢查並創建 ECR 存儲庫
# ----------------------------------------
echo "=== 檢查並創建 ECR 存儲庫 ==="
REPO_EXISTS=$(aws ecr describe-repositories \
  --repository-names "$ECR_REPOSITORY" \
  --region "$AWS_REGION" \
  --query "repositories[0].repositoryName" \
  --output text 2>/dev/null || echo "")

if [ "$REPO_EXISTS" != "$ECR_REPOSITORY" ]; then
  echo "ECR 存儲庫 $ECR_REPOSITORY 不存在，正在創建..."
  aws ecr create-repository --repository-name "$ECR_REPOSITORY" --region "$AWS_REGION"
  echo "✅ ECR 存儲庫創建完成"
else
  echo "✅ ECR 存儲庫 $ECR_REPOSITORY 已存在"
fi

# 登錄到 ECR
aws ecr get-login-password --region "$AWS_REGION" \
| docker login --username AWS --password-stdin "${AWS_ACCOUNT_ID}.dkr.ecr.${AWS_REGION}.amazonaws.com"

# ----------------------------------------
# 建立 ECS Cluster（若不存在）
# ----------------------------------------
echo "=== 檢查並創建 ECS 集群 ==="
CLUSTER_EXISTS=$(aws ecs describe-clusters \
  --clusters "$ECS_CLUSTER" \
  --region "$AWS_REGION" \
  --query "clusters[0].clusterName" \
  --output text 2>/dev/null || echo "")

if [ "$CLUSTER_EXISTS" != "$ECS_CLUSTER" ]; then
  echo "ECS 集群 $ECS_CLUSTER 不存在，正在創建..."
  aws ecs create-cluster --cluster-name "$ECS_CLUSTER" --region "$AWS_REGION"
  echo "✅ ECS 集群創建完成"
else
  echo "✅ ECS 集群 $ECS_CLUSTER 已存在"
fi

# ----------------------------------------
# 檢查 Log Group
# ----------------------------------------
echo "=== 檢查並創建 CloudWatch 日誌組 ==="
LOG_GROUP="/ecs/mirror-service"
LOG_GROUP_EXISTS=$(aws logs describe-log-groups \
  --log-group-name-prefix "$LOG_GROUP" \
  --region "$AWS_REGION" \
  --query "logGroups[0].logGroupName" \
  --output text 2>/dev/null || echo "")

if [ "$LOG_GROUP_EXISTS" != "$LOG_GROUP" ]; then
  echo "CloudWatch 日誌組不存在，正在創建..."
  aws logs create-log-group --log-group-name "$LOG_GROUP" --region "$AWS_REGION"
  echo "✅ CloudWatch 日誌組創建完成"
else
  echo "✅ CloudWatch 日誌組已存在"
fi

# ----------------------------------------
# 使用 Buildx 構建並推送到 ECR
# ----------------------------------------
echo "=== 構建並推送鏡像到 ECR （linux/amd64）==="
docker buildx build \
  --platform linux/amd64 \
  --push \
  -t "$AWS_ACCOUNT_ID.dkr.ecr.$AWS_REGION.amazonaws.com/$ECR_REPOSITORY:latest" \
  .

# ----------------------------------------
# 更新 Task Definition
# ----------------------------------------
echo "=== 更新 ECS Task Definition ==="
cp mirror-service-task-definition.json mirror-service-task-definition-updated.json

if [[ "$OSTYPE" == "darwin"* ]]; then
  sed -i '' "s/<AWS_ACCOUNT_ID>/$AWS_ACCOUNT_ID/g" mirror-service-task-definition-updated.json
  sed -i '' "s/<REGION>/$AWS_REGION/g" mirror-service-task-definition-updated.json
  sed -i '' "s/<EFS_FILE_SYSTEM_ID>/$EFS_FILE_SYSTEM_ID/g" mirror-service-task-definition-updated.json
  sed -i '' "s/<EFS_ACCESS_POINT_ID>/$EFS_ACCESS_POINT_ID/g" mirror-service-task-definition-updated.json
else
  sed -i "s/<AWS_ACCOUNT_ID>/$AWS_ACCOUNT_ID/g" mirror-service-task-definition-updated.json
  sed -i "s/<REGION>/$AWS_REGION/g" mirror-service-task-definition-updated.json
  sed -i "s/<EFS_FILE_SYSTEM_ID>/$EFS_FILE_SYSTEM_ID/g" mirror-service-task-definition-updated.json
  sed -i "s/<EFS_ACCESS_POINT_ID>/$EFS_ACCESS_POINT_ID/g" mirror-service-task-definition-updated.json
fi

TASK_DEFINITION_ARN=$(aws ecs register-task-definition \
  --cli-input-json file://mirror-service-task-definition-updated.json \
  --region "$AWS_REGION" \
  --query 'taskDefinition.taskDefinitionArn' \
  --output text)

echo "✅ 註冊新 Task Definition: $TASK_DEFINITION_ARN"

# ----------------------------------------
# 創建或更新 ECS Service
# ----------------------------------------
echo "=== 部署到 ECS Service ==="
SERVICE_STATUS=$(aws ecs describe-services \
  --cluster "$ECS_CLUSTER" \
  --services "$ECS_SERVICE" \
  --region "$AWS_REGION" \
  --query 'services[0].status' \
  --output text 2>/dev/null || echo "")

# 準備 service-registries 參數
SERVICE_REGISTRIES_ARG=""
if [ -n "$CLOUDMAP_SVC_ARN" ]; then
  SERVICE_REGISTRIES_ARG="--service-registries registryArn=${CLOUDMAP_SVC_ARN},containerName=mirror-service"
fi

if [ "$SERVICE_STATUS" = "INACTIVE" ]; then
  echo "服務已 Inactive，刪除舊 Service..."
  aws ecs delete-service \
    --cluster "$ECS_CLUSTER" \
    --service "$ECS_SERVICE" \
    --force \
    --region "$AWS_REGION"
  sleep 10
  SERVICE_STATUS=""
fi

if [ "$SERVICE_STATUS" != "ACTIVE" ]; then
  echo "服務不存在，正在新建..."
  aws ecs create-service \
    --cluster "$ECS_CLUSTER" \
    --service-name "$ECS_SERVICE" \
    --task-definition "$TASK_DEFINITION_ARN" \
    --desired-count 1 \
    --launch-type FARGATE \
    --platform-version "1.4.0" \
    --network-configuration \
       "awsvpcConfiguration={subnets=[$PRIVATE_SUBNET_1],securityGroups=[$SECURITY_GROUP],assignPublicIp=DISABLED}" \
    $SERVICE_REGISTRIES_ARG \
    --enable-execute-command \
    --region "$AWS_REGION"
  echo "✅ Service 創建中，請稍後檢查狀態"
else
  echo "服務已存在，正在滾動更新..."
  aws ecs update-service \
    --cluster "$ECS_CLUSTER" \
    --service "$ECS_SERVICE" \
    --task-definition "$TASK_DEFINITION_ARN" \
    --force-new-deployment \
    --enable-execute-command \
    --region "$AWS_REGION" \
    $SERVICE_REGISTRIES_ARG \
    --network-configuration \
       "awsvpcConfiguration={subnets=[$PRIVATE_SUBNET_1],securityGroups=[$SECURITY_GROUP],assignPublicIp=DISABLED}"
  echo "✅ 滾動更新已觸發"
fi

# 清理
rm -f mirror-service-task-definition-updated.json

echo "=== 部署完成 ==="
echo "您可以使用以下命令查看："
echo "- ECS Running Count: aws ecs describe-services --cluster $ECS_CLUSTER --services $ECS_SERVICE --region $AWS_REGION --query 'services[0].runningCount'"
echo "- Service Events:    aws ecs describe-services --cluster $ECS_CLUSTER --services $ECS_SERVICE --region $AWS_REGION --query 'services[0].events[0:5]' --output table"
echo "- 容器日誌:          aws logs describe-log-streams --log-group-name $LOG_GROUP --region $AWS_REGION --order-by LastEventTime --descending --limit 1"
echo "                     aws logs get-log-events --log-group-name $LOG_GROUP --log-stream-name <上步輸出> --limit 50 --region $AWS_REGION"
