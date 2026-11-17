-- +goose Up
-- 使用 SQLite STRICT 表；连接必须在 BEGIN 之前启用 foreign_keys，保证外键约束生效。
-- *_at 使用 UTC Unix 毫秒；业务 id 使用 UUID 字符串；摘要使用 SHA-256 十六进制。
-- 本文件是已验证的设计迁移，尚未应用到生产数据库；同一迁移由 Goose 记录并只执行一次。

-- 唯一站主账号；不参与内容同步。
CREATE TABLE admin_users (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    singleton INTEGER NOT NULL DEFAULT 1 UNIQUE CHECK (singleton = 1), -- 固定 1，唯一约束限制单站主
    username TEXT NOT NULL COLLATE NOCASE UNIQUE CHECK (length(username) BETWEEN 1 AND 80), -- 登录名，NOCASE 唯一，1–80 字符
    password_hash TEXT NOT NULL, -- 包含盐和参数的 Argon2id 编码串
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    password_changed_at INTEGER NOT NULL -- 最近改密时间；改密使现存会话失效
) STRICT;

-- 服务端会话，仅存 token 和 CSRF 的摘要。
CREATE TABLE sessions (
    token_hash BLOB PRIMARY KEY NOT NULL CHECK (length(token_hash) = 32), -- 会话 token 的 SHA-256，32 字节
    admin_id TEXT NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE, -- 站主账号 id
    csrf_hash BLOB NOT NULL CHECK (length(csrf_hash) = 32), -- 派生 CSRF token 的 SHA-256，32 字节
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    last_seen_at INTEGER NOT NULL, -- 最近会话活动时间
    expires_at INTEGER NOT NULL, -- 绝对过期时间，毫秒
    CHECK (expires_at > created_at)
) STRICT;
-- 按到期时间清理失效会话。
CREATE INDEX idx_sessions_expiry ON sessions(expires_at);
-- 按站主查找会话，支持改密后统一失效。
CREATE INDEX idx_sessions_admin ON sessions(admin_id);

-- 单例站点名称、介绍和展示时区。
CREATE TABLE site_settings (
    id INTEGER PRIMARY KEY CHECK (id = 1), -- 固定 1，仅一份站点设置
    title TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 80), -- 公开站点名称，1–80 字符
    description TEXT NOT NULL DEFAULT '' CHECK (length(description) <= 300), -- 站点介绍，最多 300 字符
    timezone TEXT NOT NULL DEFAULT 'Asia/Shanghai', -- IANA 时区名称，由服务校验
    generation INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0), -- 单调递增的业务并发版本，正整数
    updated_at INTEGER NOT NULL -- 最近变化时间，UTC Unix 毫秒
) STRICT;

-- 单例仓库配置，保存凭据别名而非密钥。
CREATE TABLE repository_config (
    id INTEGER PRIMARY KEY CHECK (id = 1), -- 固定 1，仅一个内容仓库配置
    remote_url TEXT NOT NULL, -- SSH/HTTPS 仓库地址，不可带凭据
    branch TEXT NOT NULL, -- 指定同步分支名
    content_dir TEXT NOT NULL DEFAULT 'content', -- 仓库内相对内容目录，不允许越界
    credential_ref TEXT NOT NULL, -- 服务器预置凭据的别名，不能包含密钥值
    enabled INTEGER NOT NULL DEFAULT 0 CHECK (enabled IN (0,1)), -- 是否启用自动同步，0/1
    poll_interval_seconds INTEGER NOT NULL DEFAULT 60 CHECK (poll_interval_seconds BETWEEN 30 AND 3600), -- 轮询周期，30–3600 秒，默认 60
    generation INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0), -- 单调递增的业务并发版本，正整数
    last_seen_commit TEXT, -- 最近观察到的分支 HEAD，不等于全部对象已同步
    last_checked_at INTEGER, -- 最近检查仓库时间
    last_success_at INTEGER, -- 最近成功对账时间
    updated_at INTEGER NOT NULL -- 最近变化时间，UTC Unix 毫秒
) STRICT;

-- 通过解码验证并已落盘的不可变原始图片。
CREATE TABLE asset_blobs (
    sha256 TEXT PRIMARY KEY NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'), -- 原始文件 SHA-256，小写 64 位十六进制主键
    media_type TEXT NOT NULL CHECK (media_type IN ('image/jpeg','image/png','image/webp')), -- 实际解码格式，JPEG/PNG/WebP
    byte_size INTEGER NOT NULL CHECK (byte_size BETWEEN 1 AND 10485760), -- 文件大小，原图 1–10485760 字节
    width INTEGER NOT NULL CHECK (width > 0), -- 解码后宽度，正整数像素
    height INTEGER NOT NULL CHECK (height > 0), -- 解码后高度，正整数像素
    storage_key TEXT NOT NULL UNIQUE, -- 服务生成的私有相对路径键，不接受外部任意路径
    original_name TEXT NOT NULL, -- 原文件名，仅作管理展示，不能用于拼接存储路径
    created_at INTEGER NOT NULL -- 创建时间，UTC Unix 毫秒
) STRICT;
-- 图片缩略/阅读变体及生成结果。
CREATE TABLE asset_variants (
    asset_sha256 TEXT NOT NULL REFERENCES asset_blobs(sha256) ON DELETE CASCADE, -- 被引用原图摘要
    variant TEXT NOT NULL CHECK (variant IN ('thumb','reading')), -- thumb 或 reading
    state TEXT NOT NULL CHECK (state IN ('pending','ready','failed')), -- pending/ready/failed；ready 时路径、大小、宽高都必须有值
    storage_key TEXT UNIQUE, -- 服务生成的私有相对路径键，不接受外部任意路径
    byte_size INTEGER, -- 文件大小，原图 1–10485760 字节
    width INTEGER, -- 解码后宽度，正整数像素
    height INTEGER, -- 解码后高度，正整数像素
    error_code TEXT, -- 脱敏稳定错误码；无错误为 null
    updated_at INTEGER NOT NULL, -- 最近变化时间，UTC Unix 毫秒
    PRIMARY KEY (asset_sha256, variant),
    CHECK (state <> 'ready' OR (storage_key IS NOT NULL AND byte_size IS NOT NULL AND byte_size > 0 AND width IS NOT NULL AND width > 0 AND height IS NOT NULL AND height > 0))
) STRICT;

-- 四类内容的稳定身份、生命周期和当前版本指针。
CREATE TABLE content_objects (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    kind TEXT NOT NULL CHECK (kind IN ('article','project','profile','skill')), -- article/project/profile/skill，创建后不可改
    lifecycle TEXT NOT NULL DEFAULT 'active' CHECK (lifecycle IN ('active','trash')), -- active/trash；只有文章和项目可 trash
    visibility TEXT NOT NULL DEFAULT 'unpublished' CHECK (visibility IN ('unpublished','published')), -- unpublished/published；必须同时检查 lifecycle
    published_revision_id TEXT, -- 当前或最后公开版本，非空不等于当前公开
    draft_revision_id TEXT, -- 未发布修改稿指针；无修改稿为 null
    generation INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0), -- 单调递增的业务并发版本，正整数
    first_published_at INTEGER, -- 首次实际公开时间，后续编辑保持不变
    published_updated_at INTEGER, -- 最近影响公开版本的时间
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    updated_at INTEGER NOT NULL, -- 最近变化时间，UTC Unix 毫秒
    trashed_at INTEGER, -- 进入回收站时间；active 时必须 null
    -- 同一事务可先建对象再写版本；提交时检查指针必须属于当前对象。
    FOREIGN KEY (id,published_revision_id) REFERENCES content_revisions(content_id,id) DEFERRABLE INITIALLY DEFERRED,
    -- 同一事务可先建对象再写版本；提交时检查指针必须属于当前对象。
    FOREIGN KEY (id,draft_revision_id) REFERENCES content_revisions(content_id,id) DEFERRABLE INITIALLY DEFERRED,
    CHECK (visibility <> 'published' OR (lifecycle = 'active' AND published_revision_id IS NOT NULL AND first_published_at IS NOT NULL)),
    CHECK ((lifecycle = 'trash' AND trashed_at IS NOT NULL AND visibility = 'unpublished') OR (lifecycle = 'active' AND trashed_at IS NULL)),
    CHECK (lifecycle <> 'trash' OR kind IN ('article','project'))
) STRICT;
-- 全站只允许一份个人资料对象，避免多个关于我版本互相覆盖。
CREATE UNIQUE INDEX uq_profile_singleton ON content_objects(kind) WHERE kind = 'profile';
-- 仅索引可公开对象，支持按类型和首次发布时间倒序分页。
CREATE INDEX idx_content_public ON content_objects(kind,first_published_at DESC,id DESC) WHERE lifecycle = 'active' AND visibility = 'published';
-- 支持后台按生命周期、类型及更新时间筛选内容和回收站。
CREATE INDEX idx_content_admin ON content_objects(lifecycle,kind,updated_at DESC,id DESC);

-- 不可变编辑快照；空字段允许草稿，发布另行校验。
CREATE TABLE content_revisions (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    content_id TEXT NOT NULL REFERENCES content_objects(id) ON DELETE CASCADE, -- 所属内容对象 id；线程留言板为 null，同步快照可保留历史 id
    revision_no INTEGER NOT NULL CHECK (revision_no > 0), -- 当前对象内递增版本序号
    title TEXT NOT NULL DEFAULT '' CHECK (length(title) <= 120), -- 内容标题；profile 为显示名，skill 为技能名；站点设置标题最多 80 字符
    summary TEXT NOT NULL DEFAULT '' CHECK (length(summary) <= 500), -- 摘要/短介绍/实践说明，最多 500 字符
    markdown TEXT NOT NULL DEFAULT '' CHECK (length(markdown) <= 200000), -- 原始 Markdown，最多 200000 字符，草稿可为空
    slug TEXT CHECK (slug IS NULL OR length(slug) BETWEEN 1 AND 100), -- 候选或保留的 URL 标识；合法格式由服务校验
    display_order INTEGER NOT NULL DEFAULT 0, -- 展示排序，小值在前；属于版本时发布才影响公开
    cover_sha256 TEXT REFERENCES asset_blobs(sha256) ON DELETE RESTRICT, -- 封面/头像原图摘要，可为空，删除受 FK 保护
    cover_alt TEXT NOT NULL DEFAULT '' CHECK (length(cover_alt) <= 300), -- 封面或头像说明，最多 300 字符
    source TEXT NOT NULL CHECK (source IN ('admin','repository','resolution','taxonomy')), -- admin/repository/resolution/taxonomy
    content_digest TEXT NOT NULL CHECK (length(content_digest) = 64), -- 可同步业务字段规范化 SHA-256，不含本地版本时间
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    UNIQUE (content_id,id),
    UNIQUE (content_id,revision_no)
) STRICT;
-- 反向查找封面引用，删除图片前检查保留版本。
CREATE INDEX idx_revision_cover ON content_revisions(cover_sha256);

-- 文章版本专属字段及只针对标题摘要的搜索文本。
CREATE TABLE article_revision_data (
    revision_id TEXT PRIMARY KEY NOT NULL REFERENCES content_revisions(id) ON DELETE CASCADE, -- 所属不可变版本 id
    channel TEXT CHECK (channel IN ('tech','life')), -- 文章 tech/life；标签目录 all；类别必须 tech/life
    search_title TEXT NOT NULL DEFAULT '', -- title 经过 trim 和 Unicode case fold 的派生搜索值
    search_summary TEXT NOT NULL DEFAULT '' -- summary 经过 trim 和 Unicode case fold 的派生搜索值
) STRICT;
-- 项目版本的角色、时间和技术信息。
CREATE TABLE project_revision_data (
    revision_id TEXT PRIMARY KEY NOT NULL REFERENCES content_revisions(id) ON DELETE CASCADE, -- 所属不可变版本 id
    role TEXT NOT NULL DEFAULT '' CHECK (length(role) <= 120), -- 项目中的职责，最多 120 字符
    started_on TEXT, -- 可选项目开始日期 YYYY-MM-DD
    ended_on TEXT, -- 可选项目结束日期，不早于开始日期
    featured INTEGER NOT NULL DEFAULT 0 CHECK (featured IN (0,1)), -- 是否精选，0/1
    tech_stack_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(tech_stack_json) AND json_type(tech_stack_json) = 'array'), -- 技术栈名称数组，API 限制最多 30 项，每项 40 字符
    CHECK (ended_on IS NULL OR started_on IS NULL OR ended_on >= started_on)
) STRICT;
-- 个人资料版本专属字段；名称/短介绍/头像在主版本。
CREATE TABLE profile_revision_data (
    revision_id TEXT PRIMARY KEY NOT NULL REFERENCES content_revisions(id) ON DELETE CASCADE, -- 所属不可变版本 id
    location TEXT NOT NULL DEFAULT '' CHECK (length(location) <= 120), -- 选择公开的所在地，最多 120 字符
    occupation TEXT NOT NULL DEFAULT '' CHECK (length(occupation) <= 120) -- 公开职业描述，最多 120 字符
) STRICT;
-- 技能版本的分组；名称/实践说明在主版本。
CREATE TABLE skill_revision_data (
    revision_id TEXT PRIMARY KEY NOT NULL REFERENCES content_revisions(id) ON DELETE CASCADE, -- 所属不可变版本 id
    group_name TEXT NOT NULL DEFAULT '' CHECK (length(group_name) <= 80) -- 技能分组名，最多 80 字符；发布技能必需
) STRICT;

-- 正文派生 HTML 与 H2/H3 目录，按渲染器版本保存。
CREATE TABLE revision_renderings (
    revision_id TEXT NOT NULL REFERENCES content_revisions(id) ON DELETE CASCADE, -- 所属不可变版本 id
    renderer_version TEXT NOT NULL, -- 渲染器与清洗策略联合版本号
    body_html TEXT NOT NULL, -- 统一清洗后的正文 HTML
    toc_json TEXT NOT NULL CHECK (json_valid(toc_json) AND json_type(toc_json) = 'array'), -- H2/H3 目录对象数组，包含 id/text/level
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    PRIMARY KEY (revision_id,renderer_version)
) STRICT;

-- 分类标签编辑目录，退休后仍保护历史外键。
CREATE TABLE taxonomy_items (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    kind TEXT NOT NULL CHECK (kind IN ('category','tag')), -- category/tag；与 channel 组合校验
    channel TEXT NOT NULL CHECK (channel IN ('tech','life','all')), -- 文章 tech/life；标签目录 all；类别必须 tech/life
    name TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 40), -- 分类或标签名称，目录 1–40 字符；版本关系保留快照
    normalized_name TEXT NOT NULL, -- 归一化名称，供目录唯一性判断
    slug TEXT NOT NULL CHECK (length(slug) BETWEEN 1 AND 80), -- 候选或保留的 URL 标识；合法格式由服务校验
    display_order INTEGER NOT NULL DEFAULT 0, -- 展示排序，小值在前；属于版本时发布才影响公开
    retired_at INTEGER, -- 目录退休时间，不再供新版本选择
    generation INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0), -- 单调递增的业务并发版本，正整数
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    updated_at INTEGER NOT NULL, -- 最近变化时间，UTC Unix 毫秒
    CHECK ((kind = 'tag' AND channel = 'all') OR (kind = 'category' AND channel IN ('tech','life')))
) STRICT;
-- 同类型和频道下的有效目录名称不能重复；退休项保留历史。
CREATE UNIQUE INDEX uq_taxonomy_name ON taxonomy_items(kind,channel,normalized_name) WHERE retired_at IS NULL;
-- 有效分类标签的访问标识保持唯一，避免导入产生重复目录。
CREATE UNIQUE INDEX uq_taxonomy_slug ON taxonomy_items(kind,channel,slug) WHERE retired_at IS NULL;

-- 版本使用的分类标签名称快照；最多一个分类。
CREATE TABLE revision_taxonomy (
    revision_id TEXT NOT NULL REFERENCES content_revisions(id) ON DELETE CASCADE, -- 所属不可变版本 id
    taxonomy_id TEXT NOT NULL REFERENCES taxonomy_items(id) ON DELETE RESTRICT, -- 稳定分类或标签 id
    kind TEXT NOT NULL CHECK (kind IN ('category','tag')), -- 版本快照的 category/tag 类型
    channel TEXT NOT NULL CHECK (channel IN ('tech','life','all')), -- 文章 tech/life；标签目录 all；类别必须 tech/life
    name TEXT NOT NULL, -- 分类或标签名称，目录 1–40 字符；版本关系保留快照
    slug TEXT NOT NULL, -- 候选或保留的 URL 标识；合法格式由服务校验
    position INTEGER NOT NULL DEFAULT 0, -- 当前版本内的顺序，小值在前
    PRIMARY KEY (revision_id,taxonomy_id)
) STRICT;
-- 每个版本最多一个分类，标签不受此唯一索引限制。
CREATE UNIQUE INDEX uq_revision_category ON revision_taxonomy(revision_id) WHERE kind = 'category';
-- 查找目录项影响的版本，为改名、迁移和引用检查提供依据。
CREATE INDEX idx_revision_taxonomy_target ON revision_taxonomy(taxonomy_id,revision_id);

-- Markdown 实际引用的原图集合；封面另存主版本。
CREATE TABLE revision_assets (
    revision_id TEXT NOT NULL REFERENCES content_revisions(id) ON DELETE CASCADE, -- 所属不可变版本 id
    asset_sha256 TEXT NOT NULL REFERENCES asset_blobs(sha256) ON DELETE RESTRICT, -- 被引用原图摘要
    PRIMARY KEY (revision_id,asset_sha256)
) STRICT;
-- 按图片查询版本引用，保护历史和回收站图片。
CREATE INDEX idx_revision_assets_target ON revision_assets(asset_sha256);
-- 个人公开联系方式、项目外部地址等有序链接。
CREATE TABLE revision_links (
    revision_id TEXT NOT NULL REFERENCES content_revisions(id) ON DELETE CASCADE, -- 所属不可变版本 id
    position INTEGER NOT NULL CHECK (position >= 0), -- 当前版本内的顺序，小值在前
    label TEXT NOT NULL CHECK (length(label) BETWEEN 1 AND 80), -- 公开链接标签，1–80 字符
    url TEXT NOT NULL CHECK (length(url) <= 2048), -- 链接 URI，最多 2048 字符，服务限制协议
    PRIMARY KEY (revision_id,position)
) STRICT;
-- 版本关联的其他内容 id；允许保留已删除目标的历史标识。
CREATE TABLE revision_relations (
    revision_id TEXT NOT NULL REFERENCES content_revisions(id) ON DELETE CASCADE, -- 所属不可变版本 id
    target_content_id TEXT NOT NULL, -- 相关内容 id，无 FK；公开查询过滤失效或非公开目标
    position INTEGER NOT NULL DEFAULT 0, -- 当前版本内的顺序，小值在前
    PRIMARY KEY (revision_id,target_content_id)
) STRICT;
-- target_content_id 不设外键，以保留指向已彻底删除内容的历史快照；公开查询必须过滤失效目标。
-- 按目标内容查找关联关系，便于过滤失效关联及维护检查。
CREATE INDEX idx_revision_relations_target ON revision_relations(target_content_id);

-- 已发布规范路径与旧路径；到彻底删除前保留所有权。
CREATE TABLE content_routes (
    namespace TEXT NOT NULL CHECK (namespace IN ('posts','projects')), -- posts/projects 路由空间
    slug TEXT NOT NULL, -- 候选或保留的 URL 标识；合法格式由服务校验
    content_id TEXT NOT NULL REFERENCES content_objects(id) ON DELETE CASCADE, -- 所属内容对象 id；线程留言板为 null，同步快照可保留历史 id
    is_current INTEGER NOT NULL CHECK (is_current IN (0,1)), -- 是否当前规范路径，0/1
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    PRIMARY KEY (namespace,slug)
) STRICT;
-- 每个内容对象只能有一条当前规范路由，旧路由继续保留。
CREATE UNIQUE INDEX uq_content_current_route ON content_routes(content_id) WHERE is_current = 1;
-- 按内容查找规范地址和历史别名，支持改名与彻底删除。
CREATE INDEX idx_content_routes_owner ON content_routes(content_id);

-- 根留言及平铺回复所在的线程，统一管理并发版本。
CREATE TABLE comment_threads (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    scope TEXT NOT NULL CHECK (scope IN ('article','guestbook')), -- article/guestbook；前者必须有 content_id，后者必须 null
    content_id TEXT REFERENCES content_objects(id) ON DELETE CASCADE, -- 所属内容对象 id；线程留言板为 null，同步快照可保留历史 id
    generation INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0), -- 单调递增的业务并发版本，正整数
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    updated_at INTEGER NOT NULL, -- 最近变化时间，UTC Unix 毫秒
    CHECK ((scope = 'article' AND content_id IS NOT NULL) OR (scope = 'guestbook' AND content_id IS NULL))
) STRICT;
-- 按文章或留言板来源及创建时间分页读取根留言线程。
CREATE INDEX idx_threads_source ON comment_threads(scope,content_id,created_at DESC,id DESC);
-- 根留言与回复的公开文字、目标快照及自身可见状态。
CREATE TABLE comments (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    thread_id TEXT NOT NULL REFERENCES comment_threads(id) ON DELETE CASCADE, -- 所属根留言线程
    is_root INTEGER NOT NULL CHECK (is_root IN (0,1)), -- 是否根留言，0/1；每线程最多一个根
    reply_to_id TEXT REFERENCES comments(id) ON DELETE SET NULL, -- 同线程回复目标，目标删除后设 null
    reply_to_nickname TEXT, -- 回复目标昵称快照，目标删除仍保留
    nickname TEXT NOT NULL CHECK (length(trim(nickname)) BETWEEN 1 AND 40), -- 展示昵称，trim 后 1–40 字符，不代表身份认证
    body TEXT NOT NULL CHECK (length(trim(body)) BETWEEN 1 AND 2000), -- 纯文本评论，trim 后 1–2000 字符
    author_kind TEXT NOT NULL DEFAULT 'visitor' CHECK (author_kind IN ('visitor','owner')), -- visitor/owner；只能由服务端认证结果决定
    state TEXT NOT NULL DEFAULT 'visible' CHECK (state IN ('visible','hidden')), -- visible/hidden，只记录自身状态；根和文章状态另参与公开判断
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    updated_at INTEGER NOT NULL, -- 最近变化时间，UTC Unix 毫秒
    CHECK (is_root = 0 OR (reply_to_id IS NULL AND reply_to_nickname IS NULL))
) STRICT;
-- 一个线程最多一个根留言；根和线程同时创建由服务事务保证。
CREATE UNIQUE INDEX uq_thread_root ON comments(thread_id) WHERE is_root = 1;
-- 按线程和时间读取平铺回复，id 用于同时间的稳定排序。
CREATE INDEX idx_comments_thread ON comments(thread_id,created_at,id);
-- 反向查询回复目标，支持目标删除时清空引用并保留昵称快照。
CREATE INDEX idx_comments_reply ON comments(reply_to_id);
-- 可选私密邮箱，禁止公开查询和仓库导出。
CREATE TABLE comment_contacts (
    comment_id TEXT PRIMARY KEY NOT NULL REFERENCES comments(id) ON DELETE CASCADE, -- 对应评论 id
    email TEXT NOT NULL CHECK (length(email) BETWEEN 3 AND 254) -- 私密联系邮箱，最多 254 字符，仅管理端查询
) STRICT;

-- 规范化对象同步快照；包含完整公开/草稿和删除状态。
CREATE TABLE sync_snapshots (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    content_id TEXT NOT NULL, -- 所属内容对象 id；线程留言板为 null，同步快照可保留历史 id
    digest TEXT NOT NULL CHECK (length(digest) = 64), -- 规范化同步快照 SHA-256
    payload_json TEXT NOT NULL CHECK (json_valid(payload_json) AND json_type(payload_json) = 'object'), -- 类型化 JSON 对象；快照或任务载荷，结构由服务/schema 校验
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    UNIQUE (content_id,digest),
    UNIQUE (content_id,id)
) STRICT;
-- 共同基线、冲突及解决意图所需图片的引用保护。
CREATE TABLE sync_snapshot_assets (
    snapshot_id TEXT NOT NULL REFERENCES sync_snapshots(id) ON DELETE CASCADE, -- 被引用的完整同步快照 id
    asset_sha256 TEXT NOT NULL REFERENCES asset_blobs(sha256) ON DELETE RESTRICT, -- 被引用原图摘要
    PRIMARY KEY (snapshot_id,asset_sha256)
) STRICT;
-- 按图片查找同步快照引用，防止删除冲突处理仍需要的资源。
CREATE INDEX idx_sync_asset_pins ON sync_snapshot_assets(asset_sha256);
-- 最后实际收敛的共同对象快照及仓库提交。
CREATE TABLE sync_bases (
    content_id TEXT PRIMARY KEY NOT NULL REFERENCES content_objects(id) ON DELETE CASCADE, -- 所属内容对象 id；线程留言板为 null，同步快照可保留历史 id
    snapshot_id TEXT NOT NULL, -- 被引用的完整同步快照 id
    repository_commit TEXT NOT NULL, -- 共同基线实际确认的远端 commit OID
    synced_generation INTEGER NOT NULL, -- 实际同步成功的网站 generation
    synced_at INTEGER NOT NULL, -- 共同基线确认时间
    -- 同步引用必须属于当前内容，不能把其他对象快照当作共同基线或冲突版本。
    FOREIGN KEY (content_id,snapshot_id) REFERENCES sync_snapshots(content_id,id) ON DELETE RESTRICT
) STRICT;
-- 基础、网站和仓库三方快照，保留冲突时的版本条件。
CREATE TABLE sync_conflicts (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    content_id TEXT NOT NULL REFERENCES content_objects(id) ON DELETE CASCADE, -- 所属内容对象 id；线程留言板为 null，同步快照可保留历史 id
    base_snapshot_id TEXT, -- 共同基线 B；从未收敛时为 null
    local_snapshot_id TEXT NOT NULL, -- 网站完整快照 L
    remote_snapshot_id TEXT NOT NULL, -- 仓库完整快照 R，包括不存在状态
    local_generation INTEGER NOT NULL, -- 冲突时的网站版本
    remote_commit TEXT NOT NULL, -- 形成该冲突时的仓库 HEAD
    state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open','resolving','resolved','superseded')), -- open/resolving/resolved/superseded；每对象最多一个 open/resolving
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    resolved_at INTEGER, -- 本地应用人工选择的时间
    -- 同步引用必须属于当前内容，不能把其他对象快照当作共同基线或冲突版本。
    FOREIGN KEY (content_id,base_snapshot_id) REFERENCES sync_snapshots(content_id,id) ON DELETE RESTRICT,
    -- 同步引用必须属于当前内容，不能把其他对象快照当作共同基线或冲突版本。
    FOREIGN KEY (content_id,local_snapshot_id) REFERENCES sync_snapshots(content_id,id) ON DELETE RESTRICT,
    -- 同步引用必须属于当前内容，不能把其他对象快照当作共同基线或冲突版本。
    FOREIGN KEY (content_id,remote_snapshot_id) REFERENCES sync_snapshots(content_id,id) ON DELETE RESTRICT
) STRICT;
-- 每个内容只允许一个待处理或正在解决的冲突。
CREATE UNIQUE INDEX uq_conflict_active ON sync_conflicts(content_id) WHERE state IN ('open','resolving');
-- 人工解决的明确意图与执行状态；应用后仍需确认仓库收敛。
CREATE TABLE sync_resolutions (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    conflict_id TEXT NOT NULL REFERENCES sync_conflicts(id) ON DELETE CASCADE, -- 所属冲突 id
    selection TEXT NOT NULL CHECK (selection IN ('local','remote','manual')), -- local/remote/manual
    action TEXT NOT NULL CHECK (action IN ('save_draft','publish','withdraw','trash')), -- save_draft/publish/withdraw/trash，必须显式选择
    merged_fields_json TEXT CHECK (merged_fields_json IS NULL OR (json_valid(merged_fields_json) AND json_type(merged_fields_json) = 'object')), -- manual 选择的完整修改稿；其他选择必须 null
    expected_generation INTEGER NOT NULL, -- 解决意图期待的网站版本
    expected_remote_digest TEXT NOT NULL, -- 用户确认的远端完整对象摘要
    target_snapshot_id TEXT REFERENCES sync_snapshots(id) ON DELETE RESTRICT, -- 准备/应用的目标快照，固定合并资源并保护引用
    state TEXT NOT NULL CHECK (state IN ('requested','applied','failed','superseded')), -- requested/applied/failed/superseded；applied 不等于仓库已同步
    applied_generation INTEGER, -- 解决动作真正生效后的网站 generation
    error_code TEXT, -- 脱敏稳定错误码；无错误为 null
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    updated_at INTEGER NOT NULL, -- 最近变化时间，UTC Unix 毫秒
    CHECK ((selection = 'manual') = (merged_fields_json IS NOT NULL))
) STRICT;
-- 同一冲突最多一个待执行解决意图，防止重复应用用户选择。
CREATE UNIQUE INDEX uq_resolution_pending ON sync_resolutions(conflict_id) WHERE state = 'requested';

-- 持久任务；执行租约、重试和去重，不依赖内存队列。
CREATE TABLE outbox_jobs (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    kind TEXT NOT NULL CHECK (kind IN ('sync_content','reconcile','resolve_conflict','delete_remote','image_variant','repository_check','delete_asset')), -- sync_content/reconcile/resolve_conflict/delete_remote/image_variant/repository_check/delete_asset
    aggregate_id TEXT, -- 内容/资源/解决意图 id，无 FK，以支持删除后的任务
    target_generation INTEGER, -- 本任务对应内容版本，仓库检查等任务可为空
    dedupe_key TEXT NOT NULL UNIQUE, -- 同一次任务意图唯一键
    payload_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(payload_json) AND json_type(payload_json) = 'object'), -- 类型化 JSON 对象；快照或任务载荷，结构由服务/schema 校验
    state TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','running','retry_wait','succeeded','failed','conflict','superseded')), -- queued/running/retry_wait/succeeded/failed/conflict/superseded
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0), -- 尝试次数，非负
    next_run_at INTEGER NOT NULL, -- 下次可以领取任务的时间
    lease_token TEXT, -- running 任务持有者随机 token
    lease_expires_at INTEGER, -- running 租约过期时间；非 running 必须 null
    error_code TEXT, -- 脱敏稳定错误码；无错误为 null
    error_message TEXT, -- 脱敏错误摘要，不含路径与凭据
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    updated_at INTEGER NOT NULL, -- 最近变化时间，UTC Unix 毫秒
    CHECK ((state = 'running' AND lease_token IS NOT NULL AND lease_expires_at IS NOT NULL) OR (state <> 'running' AND lease_token IS NULL AND lease_expires_at IS NULL))
) STRICT;
-- 按状态和计划时间领取到期任务，避免扫描全部历史任务。
CREATE INDEX idx_jobs_due ON outbox_jobs(state,next_run_at,id);
-- 按内容或资源查询任务历史，判断当前版本的同步进度。
CREATE INDEX idx_jobs_aggregate ON outbox_jobs(aggregate_id,created_at DESC);
-- 仓库事件去重记录，首版轮询之外的提供方适配预留。
CREATE TABLE repo_events (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    provider TEXT NOT NULL, -- 仓库事件提供方标识
    delivery_id TEXT NOT NULL, -- 提供方事件唯一标识
    config_generation INTEGER NOT NULL, -- 接收事件时仓库配置版本，防止旧配置事件执行
    branch TEXT NOT NULL, -- 指定同步分支名
    commit_oid TEXT, -- 事件提示的提交，可为空；执行仍读取当前 HEAD
    state TEXT NOT NULL CHECK (state IN ('received','processed','ignored','failed')), -- received/processed/ignored/failed
    received_at INTEGER NOT NULL, -- 事件接收时间
    processed_at INTEGER, -- 处理完成时间
    UNIQUE (provider,delivery_id)
) STRICT;
-- 彻底删除后的最小身份记录，阻止旧事件重建对象。
CREATE TABLE tombstones (
    content_id TEXT PRIMARY KEY NOT NULL, -- 所属内容对象 id；线程留言板为 null，同步快照可保留历史 id
    kind TEXT NOT NULL CHECK (kind IN ('article','project')), -- 已彻底删除的 article/project 类型
    generation INTEGER NOT NULL, -- 单调递增的业务并发版本，正整数
    purged_at INTEGER NOT NULL, -- 彻底删除时间
    remote_delete_confirmed_at INTEGER -- 远端删除确认时间
) STRICT;

-- 成功写请求的短期去重响应，与业务事务一起提交。
CREATE TABLE idempotency_records (
    scope TEXT NOT NULL, -- 用户或 anonymous + HTTP 方法 + 规范路径的命名空间
    resource_id TEXT, -- 用于删除资源时清理旧幂等响应的对象 id，无 FK
    key_hash BLOB NOT NULL CHECK (length(key_hash) = 32), -- 高熵幂等键 SHA-256，32 字节
    request_hash BLOB NOT NULL CHECK (length(request_hash) = 32), -- 规范化请求的服务端 HMAC，32 字节，不存明文邮箱载荷
    response_status INTEGER NOT NULL CHECK (response_status BETWEEN 200 AND 299), -- 原成功 HTTP 状态，200–299
    response_json TEXT NOT NULL CHECK (json_valid(response_json)), -- 原成功响应 JSON，204 使用 null JSON
    created_at INTEGER NOT NULL, -- 创建时间，UTC Unix 毫秒
    expires_at INTEGER NOT NULL, -- 绝对过期时间，毫秒
    PRIMARY KEY (scope,key_hash),
    CHECK (expires_at > created_at)
) STRICT;
-- 清理超过幂等窗口的成功响应记录。
CREATE INDEX idx_idempotency_expiry ON idempotency_records(expires_at);
-- 内容或评论删除时清理旧幂等响应，避免保留其正文副本。
CREATE INDEX idx_idempotency_resource ON idempotency_records(resource_id);
-- 脱敏操作审计，不保存正文、邮箱、密码或密钥。
CREATE TABLE audit_events (
    id TEXT PRIMARY KEY NOT NULL, -- 业务 UUID 主键
    actor_kind TEXT NOT NULL CHECK (actor_kind IN ('owner','sync','system')), -- owner/sync/system
    actor_id TEXT, -- 可选站主或任务 id，不设强关联
    action TEXT NOT NULL, -- 明确业务动作；audit 中为动作编码
    target_id TEXT, -- 审计目标 id；目标删除后仍可审计
    request_id TEXT NOT NULL, -- 对应请求或后台任务关联标识
    details_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(details_json) AND json_type(details_json) = 'object'), -- 脱敏审计元数据，仅保留版本及结果等必要信息
    created_at INTEGER NOT NULL -- 创建时间，UTC Unix 毫秒
) STRICT;
-- 按目标与时间查询操作记录，支持故障定位。
CREATE INDEX idx_audit_target ON audit_events(target_id,created_at DESC);

-- 公开投影只选择已发布版本，不关联修改稿或私密邮箱；API 仍需按公开字段允许列表序列化。
-- 公开内容视图：同时满足 active 和 published，且版本归属当前对象，草稿不能进入查询结果。
CREATE VIEW public_content AS
SELECT o.id,o.kind,o.first_published_at,o.published_updated_at,
       r.id AS revision_id,r.title,r.summary,r.markdown,r.slug,r.display_order,r.cover_sha256,r.cover_alt
FROM content_objects o JOIN content_revisions r ON r.id = o.published_revision_id AND r.content_id = o.id
WHERE o.lifecycle = 'active' AND o.visibility = 'published';

-- 公开评论视图：自身和根留言都可见，文章也必须仍公开；不连接私密邮箱表。
CREATE VIEW public_comments AS
SELECT c.id,c.thread_id,c.is_root,c.reply_to_id,c.reply_to_nickname,c.nickname,c.body,c.author_kind,c.created_at
FROM comments c
JOIN comment_threads t ON t.id = c.thread_id
JOIN comments root ON root.thread_id = t.id AND root.is_root = 1
WHERE c.state = 'visible' AND root.state = 'visible'
AND (t.scope = 'guestbook' OR EXISTS (SELECT 1 FROM public_content p WHERE p.id = t.content_id AND p.kind = 'article'));

-- +goose StatementBegin
-- 内容身份和类型创建后不可修改；技术与生活切换应修改文章版本的频道。
CREATE TRIGGER content_identity_immutable BEFORE UPDATE OF id,kind ON content_objects
BEGIN SELECT RAISE(ABORT,'content identity is immutable'); END;
-- +goose StatementEnd
-- +goose StatementBegin
-- 禁止覆盖已保存主版本，后续编辑必须插入新版本；子表不可变性由服务层补齐。
CREATE TRIGGER content_revision_immutable BEFORE UPDATE ON content_revisions
BEGIN SELECT RAISE(ABORT,'revision is immutable'); END;
-- +goose StatementEnd
-- +goose StatementBegin
-- 只有文章可绑定评论线程，项目详情不开放独立评论。
CREATE TRIGGER article_thread_kind BEFORE INSERT ON comment_threads WHEN NEW.scope = 'article'
BEGIN
    SELECT RAISE(ABORT,'comments require an article') WHERE NOT EXISTS (SELECT 1 FROM content_objects WHERE id = NEW.content_id AND kind = 'article');
END;
-- +goose StatementEnd
-- +goose StatementBegin
-- 禁止把已有线程迁往另一文章或留言板，避免评论来源被篡改。
CREATE TRIGGER thread_identity_immutable BEFORE UPDATE OF scope,content_id ON comment_threads
BEGIN SELECT RAISE(ABORT,'thread identity is immutable'); END;
-- +goose StatementEnd
-- +goose StatementBegin
-- 创建回复时校验目标属于同一线程，拒绝跨文章或跨留言板引用。
CREATE TRIGGER reply_same_thread_insert BEFORE INSERT ON comments WHEN NEW.reply_to_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT,'reply target must belong to same thread') WHERE NOT EXISTS (SELECT 1 FROM comments WHERE id = NEW.reply_to_id AND thread_id = NEW.thread_id);
END;
-- +goose StatementEnd
-- +goose StatementBegin
-- 禁止修改评论的所属线程和根身份，确保整组管理边界稳定。
CREATE TRIGGER comment_identity_immutable BEFORE UPDATE OF thread_id,is_root ON comments
BEGIN SELECT RAISE(ABORT,'comment identity is immutable'); END;
-- +goose StatementEnd
-- +goose StatementBegin
-- 修改回复目标时再次校验线程；目标删除导致的 null 引用允许保留。
CREATE TRIGGER reply_same_thread_update BEFORE UPDATE OF reply_to_id ON comments WHEN NEW.reply_to_id IS NOT NULL
BEGIN
    SELECT RAISE(ABORT,'reply target must belong to same thread') WHERE NOT EXISTS (SELECT 1 FROM comments WHERE id = NEW.reply_to_id AND thread_id = NEW.thread_id);
END;
-- +goose StatementEnd

-- 不提供破坏性 Down 迁移：优先回滚到兼容当前结构的应用版本，
-- 必要时恢复明确选择的备份；跨表业务不变量仍须由服务层事务保证。
