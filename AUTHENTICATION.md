# TEEPIN Authentication System

Complete authentication and authorization system for TEEPIN platform.

## Overview

The authentication system provides:
- **User Registration & Login**: Email/password authentication with JWT tokens
- **Multi-tenancy**: Projects/workspaces for resource isolation
- **API Keys**: Long-lived tokens for CLI/SDK usage (format: `tpk_xxxxx`)
- **Secure Storage**: PostgreSQL with bcrypt password hashing
- **Token Types**: Short-lived JWT (15min) + Long-lived API keys

## Architecture

```
┌─────────────────────────────────────────────────────┐
│              Authentication Flow                     │
├─────────────────────────────────────────────────────┤
│                                                      │
│  1. User Registers                                  │
│     POST /v1/auth/register                          │
│     ↓                                                │
│  2. User Logs In                                    │
│     POST /v1/auth/login                             │
│     Returns: JWT access token (15min)               │
│     ↓                                                │
│  3. Create Project                                  │
│     POST /v1/projects (with JWT)                    │
│     ↓                                                │
│  4. Generate API Key                                │
│     POST /v1/projects/:id/api-keys                  │
│     Returns: tpk_XXXXXXX (long-lived)              │
│     ↓                                                │
│  5. Use API Key                                     │
│     Authorization: Bearer tpk_XXXXXXX               │
│     Access instances within project                 │
│                                                      │
└─────────────────────────────────────────────────────┘
```

## Database Schema

### Auth Schema

**users** - User accounts
```sql
id            UUID PRIMARY KEY
email         VARCHAR(255) UNIQUE NOT NULL
password_hash VARCHAR(255) NOT NULL
full_name     VARCHAR(255)
email_verified BOOLEAN DEFAULT FALSE
created_at    TIMESTAMP
updated_at    TIMESTAMP
deleted_at    TIMESTAMP
```

**projects** - Workspaces/Tenants
```sql
id          UUID PRIMARY KEY
owner_id    UUID REFERENCES users(id)
name        VARCHAR(255) NOT NULL
slug        VARCHAR(255) UNIQUE NOT NULL
description TEXT
created_at  TIMESTAMP
updated_at  TIMESTAMP
deleted_at  TIMESTAMP
```

**api_keys** - API Keys for programmatic access
```sql
id         UUID PRIMARY KEY
project_id UUID REFERENCES projects(id)
user_id    UUID REFERENCES users(id)
name       VARCHAR(255) NOT NULL
key_hash   VARCHAR(255) NOT NULL        -- bcrypt hash
key_prefix VARCHAR(20) NOT NULL         -- First 12 chars (tpk_12345678)
scopes     TEXT[]                       -- Permissions array
last_used_at TIMESTAMP
expires_at   TIMESTAMP
created_at   TIMESTAMP
revoked_at   TIMESTAMP
```

**sessions** - JWT refresh tokens
```sql
id                UUID PRIMARY KEY
user_id           UUID REFERENCES users(id)
refresh_token_hash VARCHAR(255) NOT NULL
user_agent        VARCHAR(500)
ip_address        INET
expires_at        TIMESTAMP NOT NULL
created_at        TIMESTAMP
revoked_at        TIMESTAMP
```

## API Endpoints

### Public Endpoints

#### Register User
```http
POST /v1/auth/register
Content-Type: application/json

{
  "email": "user@example.com",
  "password": "secure_password123",
  "full_name": "John Doe"
}

Response 201:
{
  "id": "uuid",
  "email": "user@example.com",
  "full_name": "John Doe",
  "email_verified": false,
  "created_at": "2026-06-26T10:00:00Z",
  "updated_at": "2026-06-26T10:00:00Z"
}
```

#### Login
```http
POST /v1/auth/login
Content-Type: application/json

{
  "email": "user@example.com",
  "password": "secure_password123"
}

Response 200:
{
  "access_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "refresh_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...",
  "token_type": "Bearer",
  "expires_in": 900
}
```

### Authenticated Endpoints (Require JWT or API Key)

#### Get Current User
```http
GET /v1/auth/me
Authorization: Bearer <jwt_token>

Response 200:
{
  "id": "uuid",
  "email": "user@example.com",
  "full_name": "John Doe",
  "email_verified": false,
  "created_at": "2026-06-26T10:00:00Z",
  "updated_at": "2026-06-26T10:00:00Z"
}
```

#### Create Project
```http
POST /v1/projects
Authorization: Bearer <jwt_token>
Content-Type: application/json

{
  "name": "My Project",
  "description": "Production workloads"
}

Response 201:
{
  "id": "uuid",
  "owner_id": "uuid",
  "name": "My Project",
  "slug": "my-project",
  "description": "Production workloads",
  "created_at": "2026-06-26T10:00:00Z",
  "updated_at": "2026-06-26T10:00:00Z"
}
```

#### List Projects
```http
GET /v1/projects
Authorization: Bearer <jwt_token>

Response 200:
{
  "projects": [
    {
      "id": "uuid",
      "owner_id": "uuid",
      "name": "My Project",
      "slug": "my-project",
      "description": "Production workloads",
      "created_at": "2026-06-26T10:00:00Z",
      "updated_at": "2026-06-26T10:00:00Z"
    }
  ]
}
```

#### Create API Key
```http
POST /v1/projects/<project_id>/api-keys
Authorization: Bearer <jwt_token>
Content-Type: application/json

{
  "name": "Production API Key",
  "scopes": ["instances:read", "instances:write"]
}

Response 201:
{
  "key": "tpk_Abc123XyZ...",  // ONLY SHOWN ONCE!
  "api_key": {
    "id": "uuid",
    "project_id": "uuid",
    "user_id": "uuid",
    "name": "Production API Key",
    "key_prefix": "tpk_Abc12345",
    "scopes": ["instances:read", "instances:write"],
    "created_at": "2026-06-26T10:00:00Z"
  }
}
```

#### List API Keys
```http
GET /v1/projects/<project_id>/api-keys
Authorization: Bearer <jwt_token>

Response 200:
{
  "api_keys": [
    {
      "id": "uuid",
      "project_id": "uuid",
      "user_id": "uuid",
      "name": "Production API Key",
      "key_prefix": "tpk_Abc12345",
      "scopes": ["instances:read", "instances:write"],
      "last_used_at": "2026-06-26T10:00:00Z",
      "created_at": "2026-06-26T10:00:00Z"
    }
  ]
}
```

#### Revoke API Key
```http
DELETE /v1/projects/<project_id>/api-keys/<key_id>
Authorization: Bearer <jwt_token>

Response 200:
{
  "message": "API key revoked"
}
```

## Usage Examples

### With cURL

```bash
# 1. Register
curl -X POST http://localhost:8080/v1/auth/register \
  -H "Content-Type: application/json" \
  -d '{
    "email": "test@example.com",
    "password": "password123",
    "full_name": "Test User"
  }'

# 2. Login
TOKEN=$(curl -s -X POST http://localhost:8080/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{
    "email": "test@example.com",
    "password": "password123"
  }' | jq -r '.access_token')

# 3. Create Project
PROJECT_ID=$(curl -s -X POST http://localhost:8080/v1/projects \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "name": "My Project",
    "description": "Test project"
  }' | jq -r '.id')

# 4. Create API Key
API_KEY=$(curl -s -X POST http://localhost:8080/v1/projects/$PROJECT_ID/api-keys \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "name": "CLI Key",
    "scopes": ["instances:read", "instances:write"]
  }' | jq -r '.key')

# 5. Use API Key
curl -X GET http://localhost:8080/v1/compute/instance-types \
  -H "Authorization: Bearer $API_KEY"
```

### With Go SDK

```go
// Coming soon: SDK will be updated to support authentication
```

### With Python SDK

```python
# Coming soon: SDK will be updated to support authentication
```

### With TypeScript SDK

```typescript
// Coming soon: SDK will be updated to support authentication
```

## Security Features

### Password Security
- **bcrypt hashing**: Passwords hashed with bcrypt (cost factor 10)
- **Never logged**: Passwords never appear in logs or responses
- **Minimum length**: 8 characters required

### Token Security
- **JWT**: Short-lived (15 minutes), signed with HS256
- **API Keys**: bcrypt hashed, only full key shown once
- **Secure generation**: Cryptographically secure random bytes
- **Revocation**: API keys can be revoked instantly

### Database Security
- **Prepared statements**: All queries use parameterized statements
- **No SQL injection**: Protected by lib/pq parameter binding
- **Soft deletes**: Users/projects marked deleted, not removed

### Network Security
- **HTTPS recommended**: Use TLS in production
- **CORS configured**: Cross-origin requests handled properly
- **Rate limiting**: (Coming soon)

## Configuration

### Environment Variables

```bash
# Database (Required)
DB_HOST=postgres.teepin.svc.cluster.local
DB_PORT=5432
DB_USER=teepin
DB_PASSWORD=<secure_password>
DB_NAME=teepin_db
DB_SSLMODE=disable  # Use 'require' in production

# JWT (Required)
JWT_SECRET=<long_random_secret>  # Change in production!

# API Server
PORT=8080
GIN_MODE=release
```

### Production Recommendations

1. **JWT_SECRET**: Use a strong, random secret (minimum 32 characters)
   ```bash
   JWT_SECRET=$(openssl rand -base64 32)
   ```

2. **Database**: Use AWS RDS with SSL enabled
   ```bash
   DB_HOST=teepin-db.xxxxx.rds.amazonaws.com
   DB_SSLMODE=require
   ```

3. **HTTPS**: Always use TLS in production
4. **Backups**: Automated daily PostgreSQL backups
5. **Monitoring**: Track failed login attempts

## Testing

### Run Automated Tests

```bash
cd /mnt/e/Data/Projects/TeepinServices/teepin-core

# Make script executable
chmod +x scripts/test-auth-system.sh

# Run tests
./scripts/test-auth-system.sh
```

### Manual Testing

```bash
# Start port-forward
kubectl port-forward -n teepin svc/teepin-api 8080:80

# Test health
curl http://localhost:8080/health

# Test registration
curl -X POST http://localhost:8080/v1/auth/register \
  -H "Content-Type: application/json" \
  -d '{"email":"test@example.com","password":"password123","full_name":"Test User"}'

# Test login
curl -X POST http://localhost:8080/v1/auth/login \
  -H "Content-Type: application/json" \
  -d '{"email":"test@example.com","password":"password123"}'
```

## Troubleshooting

### Database Connection Failed

```bash
# Check PostgreSQL is running
kubectl get pods -n teepin | grep postgres

# Check database logs
kubectl logs -n teepin postgres-xxxxx

# Test connection from API pod
kubectl exec -n teepin-system teepin-api-xxxxx -- \
  psql -h postgres.teepin.svc.cluster.local -U teepin -d teepin_db -c "SELECT 1;"
```

### Invalid Credentials

- Check password meets minimum length (8 characters)
- Verify email is correctly formatted
- Ensure user exists (check auth.users table)

### API Key Not Working

- Verify key starts with `tpk_`
- Check key hasn't been revoked (revoked_at IS NULL)
- Ensure key hasn't expired
- Verify project_id matches

## Next Steps

1. **CLI Integration**: Update `teepin` CLI to support authentication
2. **SDK Integration**: Update Go/Python/TypeScript SDKs
3. **Rate Limiting**: Add rate limiting to prevent abuse
4. **Email Verification**: Send verification emails
5. **Password Reset**: Implement forgot password flow
6. **OAuth Integration**: Add GitHub/Google OAuth

## References

- [JWT Specification](https://datatracker.ietf.org/doc/html/rfc7519)
- [bcrypt](https://en.wikipedia.org/wiki/Bcrypt)
- [OWASP Auth Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)
