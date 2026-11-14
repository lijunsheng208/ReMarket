#!/bin/bash
set -euo pipefail
# 重复执行仅补建缺失 Topic，不删除消息或修改已有分区。
for aggregate in product order; do
  for lane in events events.retry events.dlt; do
    retention="$KAFKA_RETENTION_MS"
    if [[ "$lane" == events.dlt ]]; then retention="$KAFKA_DLT_RETENTION_MS"; fi
    /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:19092 \
      --create --if-not-exists --topic "remarket.$aggregate.$lane.v1" \
      --partitions "$KAFKA_TOPIC_PARTITIONS" --replication-factor 1 \
      --config "retention.ms=$retention" --config cleanup.policy=delete
  done
done
/opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:19092 --describe
