-- ============================================================================
-- YunNian 云辇 · 数据库表结构参考（24 张表）
-- ============================================================================
--
-- 说明：本文件是「结构参考」，**不是初始化脚本**。后端首次启动时会通过
-- GORM AutoMigrate 自动建表，无需手工导入本文件。这里把它导出，是为了让
-- 想要部署的人在不启动服务的情况下，也能看清表结构、索引与外键。
--
-- 生成方式：mysqldump --no-data（仅结构、不含任何数据），从一次真实启动后的
-- MySQL 8.0 库导出。数据（默认账号/角色/权限/演示项目等）由 backend/internal/store
-- 的 Seed() 在首次启动时自动灌入，见 README「快速部署」。
--
-- 默认账号：admin / ops / guest，密码均为 changeme（见 seed.go 的 seedPassword）。
-- ============================================================================

/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
/*!40101 SET @OLD_CHARACTER_SET_RESULTS=@@CHARACTER_SET_RESULTS */;
/*!40101 SET @OLD_COLLATION_CONNECTION=@@COLLATION_CONNECTION */;
/*!50503 SET NAMES utf8mb4 */;
/*!40103 SET @OLD_TIME_ZONE=@@TIME_ZONE */;
/*!40103 SET TIME_ZONE='+00:00' */;
/*!40014 SET @OLD_UNIQUE_CHECKS=@@UNIQUE_CHECKS, UNIQUE_CHECKS=0 */;
/*!40014 SET @OLD_FOREIGN_KEY_CHECKS=@@FOREIGN_KEY_CHECKS, FOREIGN_KEY_CHECKS=0 */;
/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;
/*!40111 SET @OLD_SQL_NOTES=@@SQL_NOTES, SQL_NOTES=0 */;

-- Table structure for table `ai_model_config`

DROP TABLE IF EXISTS `ai_model_config`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `ai_model_config` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(64) COLLATE utf8mb4_general_ci NOT NULL,
  `vendor` varchar(32) COLLATE utf8mb4_general_ci NOT NULL,
  `base_url` varchar(255) COLLATE utf8mb4_general_ci NOT NULL,
  `api_key_masked` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `api_key_cipher` varbinary(4096) DEFAULT NULL,
  `api_key_iv` varbinary(32) DEFAULT NULL,
  `api_key_auth_tag` varbinary(32) DEFAULT NULL,
  `models` json DEFAULT NULL,
  `default_model` varchar(96) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `is_default` tinyint(1) NOT NULL DEFAULT '0',
  `timeout_sec` bigint NOT NULL DEFAULT '60',
  `max_concurrency` bigint NOT NULL DEFAULT '3',
  `temperature` decimal(3,2) NOT NULL DEFAULT '0.70',
  `max_tokens` bigint NOT NULL DEFAULT '2048',
  `status` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'unchecked',
  `latency_ms` bigint DEFAULT NULL,
  `checked_at` datetime(3) DEFAULT NULL,
  `call_count` int unsigned NOT NULL DEFAULT '0',
  `fail_count` int unsigned NOT NULL DEFAULT '0',
  `description` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB AUTO_INCREMENT=6 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `audit_log`

DROP TABLE IF EXISTS `audit_log`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `audit_log` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned DEFAULT NULL,
  `username` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `user_name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `role_code` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `module` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `action` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `target_type` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `target_id` bigint unsigned DEFAULT NULL,
  `target_name` varchar(191) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `detail` varchar(2000) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `diff` json DEFAULT NULL,
  `result` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'success',
  `error_msg` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `ip` varchar(45) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `user_agent` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `trace_id` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `duration_ms` int unsigned DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_audit_user_time` (`user_id`),
  KEY `idx_audit_module_action` (`module`,`action`),
  KEY `idx_audit_target` (`target_type`,`target_id`),
  KEY `idx_audit_time` (`created_at`)
) ENGINE=InnoDB AUTO_INCREMENT=812 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `build_log`

DROP TABLE IF EXISTS `build_log`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `build_log` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `build_id` bigint unsigned NOT NULL,
  `seq` int unsigned NOT NULL,
  `level` varchar(8) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'info',
  `content` varchar(2000) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `logged_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_log_seq` (`build_id`,`seq`),
  KEY `idx_log_build` (`build_id`)
) ENGINE=InnoDB AUTO_INCREMENT=9034 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `build_record`

DROP TABLE IF EXISTS `build_record`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `build_record` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `build_no` int unsigned NOT NULL,
  `project_id` bigint unsigned NOT NULL,
  `project_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `branch` varchar(96) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `commit_id` varchar(40) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `commit_msg` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `trigger_type` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'manual',
  `trigger_by` bigint unsigned DEFAULT NULL,
  `trigger_name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `params_snapshot` json DEFAULT NULL,
  `status` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'queued',
  `stage` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `progress` tinyint unsigned NOT NULL DEFAULT '0',
  `duration_sec` int unsigned DEFAULT NULL,
  `image_record_id` bigint unsigned DEFAULT NULL,
  `log_size` int unsigned NOT NULL DEFAULT '0',
  `log_url` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `error_code` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `ai_diagnosis` json DEFAULT NULL,
  `idempotency_key` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `started_at` datetime(3) DEFAULT NULL,
  `finished_at` datetime(3) DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  `jenkins_job` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `jenkins_build_no` int unsigned DEFAULT NULL,
  `jenkins_queue_id` bigint unsigned DEFAULT NULL,
  `error_message` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_build_no` (`project_id`,`build_no`),
  UNIQUE KEY `uk_build_idem` (`idempotency_key`),
  KEY `idx_build_project_time` (`project_id`,`created_at`),
  KEY `idx_build_commit` (`project_key`,`commit_id`),
  KEY `idx_build_status_time` (`status`,`created_at`),
  KEY `idx_build_jenkins` (`jenkins_job`)
) ENGINE=InnoDB AUTO_INCREMENT=33 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `deploy_param`

DROP TABLE IF EXISTS `deploy_param`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `deploy_param` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `service_id` bigint unsigned NOT NULL,
  `env_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `project_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `version` int unsigned NOT NULL,
  `replicas` bigint DEFAULT NULL,
  `cpu_request` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `cpu_limit` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `mem_request` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `mem_limit` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `hpa_enabled` tinyint(1) DEFAULT NULL,
  `env_vars` json DEFAULT NULL,
  `config_maps` json DEFAULT NULL,
  `secrets` json DEFAULT NULL,
  `diff` json DEFAULT NULL,
  `changed_by` bigint unsigned DEFAULT NULL,
  `changed_at` datetime(3) DEFAULT NULL,
  `note` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_param_ver` (`service_id`,`version`),
  KEY `idx_param_env_proj` (`service_id`,`env_key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `deploy_record`

DROP TABLE IF EXISTS `deploy_record`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `deploy_record` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `deploy_no` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `env_id` bigint unsigned NOT NULL,
  `env_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `project_id` bigint unsigned NOT NULL,
  `project_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `service_id` bigint unsigned NOT NULL,
  `image_record_id` bigint unsigned NOT NULL,
  `image` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `replicas` bigint NOT NULL DEFAULT '1',
  `strategy` varchar(24) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'RollingUpdate',
  `env_overrides` json DEFAULT NULL,
  `remark` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `status` varchar(24) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'queued',
  `progress` tinyint unsigned NOT NULL DEFAULT '0',
  `operator_id` bigint unsigned NOT NULL,
  `operator_name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `approver_id` bigint unsigned DEFAULT NULL,
  `approver_name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `approved_at` datetime(3) DEFAULT NULL,
  `reject_reason` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `parent_deploy_id` bigint unsigned DEFAULT NULL,
  `error_code` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `ai_diagnosis` json DEFAULT NULL,
  `idempotency_key` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `started_at` datetime(3) DEFAULT NULL,
  `finished_at` datetime(3) DEFAULT NULL,
  `duration_sec` int unsigned DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  `error_message` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_deploy_no` (`deploy_no`),
  UNIQUE KEY `uk_deploy_idem` (`idempotency_key`),
  KEY `idx_deploy_env_time` (`env_id`,`created_at`),
  KEY `idx_deploy_project` (`project_id`),
  KEY `idx_deploy_service` (`service_id`,`created_at`),
  KEY `idx_deploy_status` (`status`,`created_at`)
) ENGINE=InnoDB AUTO_INCREMENT=26 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `deploy_step`

DROP TABLE IF EXISTS `deploy_step`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `deploy_step` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `deploy_id` bigint unsigned NOT NULL,
  `seq` tinyint unsigned NOT NULL,
  `name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `status` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'pending',
  `detail` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `started_at` datetime(3) DEFAULT NULL,
  `finished_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_step_seq` (`deploy_id`,`seq`)
) ENGINE=InnoDB AUTO_INCREMENT=89 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `environment`

DROP TABLE IF EXISTS `environment`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `environment` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `env_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `cluster` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `cluster_version` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `namespace` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `kubeconfig_credential_id` bigint unsigned DEFAULT NULL,
  `deploy_method` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'k8s',
  `helm_chart` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `registry` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `ingress_domain` varchar(191) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `image_pull_secret` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `need_approval` tinyint(1) NOT NULL DEFAULT '0',
  `approver_ids` json DEFAULT NULL,
  `owner_id` bigint unsigned NOT NULL,
  `description` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `health_status` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'unknown',
  `cpu_usage` tinyint unsigned NOT NULL DEFAULT '0',
  `mem_usage` tinyint unsigned NOT NULL DEFAULT '0',
  `node_count` int unsigned NOT NULL DEFAULT '0',
  `last_sync_at` datetime(3) DEFAULT NULL,
  `created_by` bigint unsigned DEFAULT NULL,
  `updated_by` bigint unsigned DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  `stage_label` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `stage_order` bigint DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_env_key` (`env_key`),
  KEY `idx_env_enabled` (`enabled`),
  KEY `idx_environment_deleted_at` (`deleted_at`)
) ENGINE=InnoDB AUTO_INCREMENT=36 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `environment_project`

DROP TABLE IF EXISTS `environment_project`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `environment_project` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `env_id` bigint unsigned NOT NULL,
  `project_id` bigint unsigned NOT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_env_project` (`env_id`,`project_id`),
  KEY `idx_ep_project` (`project_id`)
) ENGINE=InnoDB AUTO_INCREMENT=22 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `environment_var`

DROP TABLE IF EXISTS `environment_var`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `environment_var` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `env_id` bigint unsigned NOT NULL,
  `var_key` varchar(128) COLLATE utf8mb4_general_ci NOT NULL,
  `var_value` varchar(2000) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `is_secret` tinyint(1) NOT NULL DEFAULT '0',
  `description` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_env_var` (`env_id`,`var_key`)
) ENGINE=InnoDB AUTO_INCREMENT=26 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `image_record`

DROP TABLE IF EXISTS `image_record`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `image_record` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `project_id` bigint unsigned NOT NULL,
  `project_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `registry` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `repo` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `tag` varchar(191) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `image` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `digest` varchar(80) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `size_bytes` bigint unsigned DEFAULT NULL,
  `arch` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `branch` varchar(96) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `commit_id` varchar(40) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `build_id` bigint unsigned NOT NULL,
  `build_no` int unsigned NOT NULL,
  `pushed_at` datetime(3) DEFAULT NULL,
  `pull_count` int unsigned NOT NULL DEFAULT '0',
  `deployed_envs` json DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_image` (`registry`,`repo`,`tag`),
  KEY `idx_image_project_time` (`project_id`,`pushed_at`),
  KEY `idx_image_commit` (`project_key`,`commit_id`),
  KEY `idx_image_build` (`build_id`),
  KEY `idx_image_record_deleted_at` (`deleted_at`)
) ENGINE=InnoDB AUTO_INCREMENT=15 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `k8s_cluster`

DROP TABLE IF EXISTS `k8s_cluster`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `k8s_cluster` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `cluster_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `name` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `api_server` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `context_name` varchar(191) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `kubeconfig_cluster` varchar(191) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `kubeconfig_credential_id` bigint unsigned DEFAULT NULL,
  `status` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'unprobed',
  `k8s_version` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `nodes` bigint DEFAULT NULL,
  `nodes_ready` bigint DEFAULT NULL,
  `namespace_count` bigint DEFAULT NULL,
  `last_probe_message` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `last_probe_ms` bigint DEFAULT NULL,
  `last_probed_at` datetime(3) DEFAULT NULL,
  `remark` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `created_by` bigint unsigned DEFAULT NULL,
  `updated_by` bigint unsigned DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_cluster_key` (`cluster_key`),
  KEY `idx_k8s_cluster_deleted_at` (`deleted_at`)
) ENGINE=InnoDB AUTO_INCREMENT=11 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `notify_channel`

DROP TABLE IF EXISTS `notify_channel`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `notify_channel` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `type` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `target` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `events` json DEFAULT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `last_sent_at` datetime(3) DEFAULT NULL,
  `fail_count` int unsigned NOT NULL DEFAULT '0',
  `created_by` bigint unsigned DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  `last_error` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_notify_enabled` (`enabled`)
) ENGINE=InnoDB AUTO_INCREMENT=5 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `permission`

DROP TABLE IF EXISTS `permission`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `permission` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `code` varchar(96) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `module` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `description` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_perm_code` (`code`),
  KEY `idx_perm_module` (`module`)
) ENGINE=InnoDB AUTO_INCREMENT=31 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `project`

DROP TABLE IF EXISTS `project`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `project` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `project_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `name` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `type` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `git_provider` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'gitlab',
  `git_url` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `git_credential_id` bigint unsigned DEFAULT NULL,
  `default_branch` varchar(96) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'master',
  `build_method` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'dockerfile',
  `jenkins_job` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `dockerfile_path` varchar(255) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'Dockerfile',
  `build_context` varchar(255) COLLATE utf8mb4_general_ci NOT NULL DEFAULT '.',
  `build_script` text COLLATE utf8mb4_general_ci,
  `registry` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `image_name` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `tag_pattern` varchar(128) COLLATE utf8mb4_general_ci NOT NULL DEFAULT '{branch}-{commitId}-{timestamp}',
  `build_params` json DEFAULT NULL,
  `timeout_sec` bigint NOT NULL DEFAULT '1800',
  `owner_id` bigint unsigned DEFAULT NULL,
  `team` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `description` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `auto_build` tinyint(1) NOT NULL DEFAULT '0',
  `auto_build_branches` json DEFAULT NULL,
  `created_by` bigint unsigned DEFAULT NULL,
  `updated_by` bigint unsigned DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  `build_image` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `build_artifact` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `artifact_dest` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `runtime_image` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `run_command` varchar(1024) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `registry_credential_id` bigint unsigned DEFAULT NULL,
  `git_username` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_project_key` (`project_key`),
  KEY `idx_project_name` (`name`),
  KEY `idx_project_type_enabled` (`type`,`enabled`),
  KEY `idx_project_owner` (`owner_id`),
  KEY `idx_project_deleted_at` (`deleted_at`)
) ENGINE=InnoDB AUTO_INCREMENT=39 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `project_member`

DROP TABLE IF EXISTS `project_member`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `project_member` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `project_id` bigint unsigned NOT NULL,
  `user_id` bigint unsigned NOT NULL,
  `role` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'developer',
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_pm` (`project_id`,`user_id`),
  KEY `idx_pm_user` (`user_id`)
) ENGINE=InnoDB AUTO_INCREMENT=15 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `role`

DROP TABLE IF EXISTS `role`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `role` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `code` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `description` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `builtin` tinyint(1) NOT NULL DEFAULT '1',
  `sort` bigint NOT NULL DEFAULT '0',
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_role_code` (`code`)
) ENGINE=InnoDB AUTO_INCREMENT=6 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `role_permission`

DROP TABLE IF EXISTS `role_permission`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `role_permission` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `role_id` bigint unsigned NOT NULL,
  `permission_id` bigint unsigned NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_role_perm` (`role_id`,`permission_id`)
) ENGINE=InnoDB AUTO_INCREMENT=23 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `rollback_record`

DROP TABLE IF EXISTS `rollback_record`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `rollback_record` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `deploy_id` bigint unsigned NOT NULL,
  `deploy_no` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `env_id` bigint unsigned NOT NULL,
  `env_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `project_id` bigint unsigned NOT NULL,
  `project_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `service_id` bigint unsigned NOT NULL,
  `from_image_id` bigint unsigned NOT NULL,
  `from_image` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `to_image_id` bigint unsigned NOT NULL,
  `to_image` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `reason` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `trigger_type` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'manual',
  `ai_confidence` decimal(4,3) DEFAULT NULL,
  `operator_id` bigint unsigned NOT NULL,
  `operator_name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `status` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'progressing',
  `duration_sec` int unsigned DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_rbk_env_time` (`env_id`,`created_at`),
  KEY `idx_rbk_service` (`service_id`,`created_at`)
) ENGINE=InnoDB AUTO_INCREMENT=5 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `secret_credential`

DROP TABLE IF EXISTS `secret_credential`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `secret_credential` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `name` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `type` varchar(32) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `scope` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `cipher` varbinary(8192) NOT NULL,
  `iv` varbinary(32) NOT NULL,
  `auth_tag` varbinary(32) NOT NULL,
  `key_version` bigint NOT NULL DEFAULT '1',
  `expires_at` datetime(3) DEFAULT NULL,
  `last_used_at` datetime(3) DEFAULT NULL,
  `description` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `created_by` bigint unsigned DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_cred_scope_name` (`scope`,`name`),
  KEY `idx_cred_expires` (`expires_at`)
) ENGINE=InnoDB AUTO_INCREMENT=40 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `service_instance`

DROP TABLE IF EXISTS `service_instance`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `service_instance` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `env_id` bigint unsigned NOT NULL,
  `project_id` bigint unsigned NOT NULL,
  `env_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `project_key` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `workload_kind` varchar(24) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'Deployment',
  `workload_name` varchar(191) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `current_image_id` bigint unsigned DEFAULT NULL,
  `current_image` varchar(512) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `current_tag` varchar(191) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `replicas` bigint NOT NULL DEFAULT '1',
  `ready_replicas` bigint NOT NULL DEFAULT '0',
  `status` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'pending',
  `cpu_request` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `cpu_limit` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `mem_request` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `mem_limit` varchar(16) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `env_vars` json DEFAULT NULL,
  `config_maps` json DEFAULT NULL,
  `secrets` json DEFAULT NULL,
  `entrypoint` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `hpa_enabled` tinyint(1) NOT NULL DEFAULT '0',
  `liveness_ok` tinyint(1) NOT NULL DEFAULT '1',
  `readiness_ok` tinyint(1) NOT NULL DEFAULT '1',
  `applied_revision` bigint DEFAULT NULL,
  `last_deploy_id` bigint unsigned DEFAULT NULL,
  `updated_by` bigint unsigned DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  `deleted_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_svc_env_project` (`env_id`,`project_id`),
  KEY `idx_svc_project` (`project_id`,`project_key`),
  KEY `idx_svc_env_status` (`env_key`,`status`),
  KEY `idx_service_instance_deleted_at` (`deleted_at`)
) ENGINE=InnoDB AUTO_INCREMENT=14 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `system_config`

DROP TABLE IF EXISTS `system_config`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `system_config` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `config_key` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `config_value` text COLLATE utf8mb4_general_ci,
  `value_type` varchar(16) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'string',
  `group_name` varchar(64) COLLATE utf8mb4_general_ci NOT NULL DEFAULT 'common',
  `description` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `updated_by` bigint unsigned DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_config_key` (`config_key`)
) ENGINE=InnoDB AUTO_INCREMENT=25 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `user`

DROP TABLE IF EXISTS `user`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `user` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `username` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `password_hash` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `name` varchar(64) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `email` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `phone` varchar(20) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `avatar` varchar(255) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `sso_subject` varchar(128) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `enabled` tinyint(1) NOT NULL DEFAULT '1',
  `last_login_at` datetime(3) DEFAULT NULL,
  `last_login_ip` varchar(45) COLLATE utf8mb4_general_ci DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `updated_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_username` (`username`),
  UNIQUE KEY `uk_user_email` (`email`),
  UNIQUE KEY `uk_user_sso` (`sso_subject`)
) ENGINE=InnoDB AUTO_INCREMENT=23 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

-- Table structure for table `user_role`

DROP TABLE IF EXISTS `user_role`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `user_role` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `role_id` bigint unsigned NOT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_user_role` (`user_id`,`role_id`),
  KEY `idx_ur_role` (`user_id`)
) ENGINE=InnoDB AUTO_INCREMENT=23 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;
/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;

-- Dump completed on 2026-10-12 17:40:20
