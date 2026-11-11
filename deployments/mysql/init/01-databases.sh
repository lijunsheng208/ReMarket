#!/bin/bash
# Sourced by the official MySQL entrypoint on first initialization only.
# Passwords are hex-encoded before SQL interpolation, including quotes/backslashes.
for service in user product order chat; do
  variable="MYSQL_${service^^}_PASSWORD"
  password="${!variable:?Missing database password}"
  password_hex=$(printf '%s' "$password" | od -An -v -tx1 | tr -d ' \n')
  docker_process_sql <<SQL
CREATE DATABASE IF NOT EXISTS remarket_${service} CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci;
SET @password = CONVERT(0x${password_hex} USING utf8mb4);
SET @statement = CONCAT('CREATE USER IF NOT EXISTS remarket_${service}@''%'' IDENTIFIED BY ', QUOTE(@password));
PREPARE create_account FROM @statement;
EXECUTE create_account;
DEALLOCATE PREPARE create_account;
GRANT SELECT, INSERT, UPDATE, DELETE ON remarket_${service}.* TO 'remarket_${service}'@'%';
SQL
done
