-- imgbed 元数据库表结构（MySQL 8.0+ / OCI HeatWave）
--
-- 与 SQLite 版本的三个关键差异，改动前务必先读：
--
--   1. parent_id / folder_id 用空串表示「根目录」而不是 NULL。
--      唯一索引里多个 NULL 互不相同，用 NULL 会让同名根目录被重复创建。
--
--   2. MySQL 不支持带 WHERE 的部分索引，因此「未删除的图片按内容去重」
--      改用 STORED 生成列 dedup_key（未删除时等于 sha256，已删除时为 NULL）。
--      唯一索引中多个 NULL 不冲突，正好达到同样的效果。
--
--   3. object_key 与 sha256 必须用二进制排序规则。
--      MySQL 默认的 utf8mb4_0900_ai_ci 大小写不敏感，会把 Photo.jpg
--      和 photo.jpg 判定为冲突；对象存储的 key 是大小写敏感的。
--
-- 所有时间戳都是 BIGINT 秒级 Unix 时间，由 Go 侧生成，不使用 MySQL 的时间类型。

CREATE TABLE IF NOT EXISTS users (
  id           VARCHAR(64)  NOT NULL COMMENT 'Ucenter 的 sub',
  email        VARCHAR(255) NOT NULL DEFAULT '',
  role         VARCHAR(16)  NOT NULL DEFAULT 'user',
  created_at   BIGINT       NOT NULL,
  last_seen_at BIGINT       NOT NULL,
  PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS folders (
  id         VARCHAR(26)  NOT NULL COMMENT 'ULID',
  owner_id   VARCHAR(64)  NOT NULL,
  parent_id  VARCHAR(26)  NOT NULL DEFAULT '' COMMENT '空串 = 根目录',
  name       VARCHAR(255) NOT NULL,
  path       VARCHAR(700) NOT NULL DEFAULT '' COMMENT '物化路径，如 风景/2026',
  migrating  TINYINT(1)   NOT NULL DEFAULT 0,
  created_at BIGINT       NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_folders_sibling (owner_id, parent_id, name),
  KEY idx_folders_path (owner_id, path(191))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS images (
  id          VARCHAR(26)  NOT NULL COMMENT 'ULID',
  owner_id    VARCHAR(64)  NOT NULL,
  folder_id   VARCHAR(26)  NOT NULL DEFAULT '' COMMENT '空串 = 根目录',
  object_key  VARCHAR(700) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL COMMENT 'R2 对象 key',
  filename    VARCHAR(255) NOT NULL COMMENT '展示名，改它不影响 R2 key',
  ext         VARCHAR(16)  NOT NULL DEFAULT '',
  mime        VARCHAR(128) NOT NULL DEFAULT '',
  size        BIGINT       NOT NULL DEFAULT 0,
  width       INT          NOT NULL DEFAULT 0,
  height      INT          NOT NULL DEFAULT 0,
  sha256      CHAR(64)     CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '用于去重/秒传',
  visibility  VARCHAR(16)  NOT NULL DEFAULT 'public' COMMENT 'public | private',
  views       BIGINT       NOT NULL DEFAULT 0,
  deleted_at  BIGINT       NULL COMMENT '软删除标记，NULL = 正常',
  created_at  BIGINT       NOT NULL,
  dedup_key   VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin
              GENERATED ALWAYS AS (IF(deleted_at IS NULL, sha256, NULL)) STORED,
  PRIMARY KEY (id),
  UNIQUE KEY uk_images_object_key (object_key),
  UNIQUE KEY uk_images_dedup (owner_id, dedup_key),
  KEY idx_images_list (owner_id, folder_id, deleted_at, created_at),
  KEY idx_images_trash (owner_id, deleted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS jobs (
  id         VARCHAR(26) NOT NULL COMMENT 'ULID',
  owner_id   VARCHAR(64) NOT NULL,
  kind       VARCHAR(32) NOT NULL COMMENT '如 folder_migrate',
  payload    MEDIUMTEXT  NOT NULL,
  total      INT         NOT NULL DEFAULT 0,
  done       INT         NOT NULL DEFAULT 0,
  status     VARCHAR(16) NOT NULL DEFAULT 'pending' COMMENT 'pending | running | done | failed',
  error      TEXT        NOT NULL,
  created_at BIGINT      NOT NULL,
  updated_at BIGINT      NOT NULL,
  PRIMARY KEY (id),
  KEY idx_jobs_owner (owner_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
