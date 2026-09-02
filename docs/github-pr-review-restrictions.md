# GitHub PR Review Restrictions: Step-by-Step Guide

This guide shows you how to restrict who can review and approve pull requests in your GitHub repository using **CODEOWNERS** and **Branch Protection Rules**.

---

## 📋 Overview

| Method | Purpose | Required Plan |
|--------|---------|---------------|
| [CODEOWNERS](#1-set-up-codeowners) | Auto-request reviews from specific users/teams based on file paths | Free |
| [Branch Protection](#2-configure-branch-protection) | Enforce approval requirements before merging | Free |
| [Teams (Optional)](#optional-create-github-teams) | Organize reviewers into groups | Free for Orgs |

---

## ✅ Step 1: Set Up CODEOWNERS

CODEOWNERS automatically requests reviews from specific people or teams when files matching certain patterns are changed.

### 1.1 Create the CODEOWNERS File

1. In your repository, click **"Add file"** → **"Create new file"**
2. In the file name field, type: `.github/CODEOWNERS`
3. Add your rules. Examples:

   ```plaintext
   # Request reviews from @alice and @bob for ALL files
   * @alice @bob
   
   # Request reviews from @security-team for security-related files
   /security/ @org/security-team
   
   # Request reviews from @frontend-team for frontend code
   src/frontend/ @org/frontend-team
   
   # Request reviews from @backend-team for backend code
   src/backend/ @org/backend-team
   
   # Request reviews from @devops-team for infrastructure files
   /terraform/ @org/devops-team
   /k8s/ @org/devops-team
   Dockerfile @org/devops-team
   ```

   **Rule Syntax:**
   - `*` = matches all files
   - `/path/` = matches files in that directory
   - `filename` = matches specific file
   - `@username` = individual user
   - `@org/team-name` = GitHub team

4. Click **"Commit new file"**

### 1.2 Verify CODEOWNERS is Working

1. Create a test pull request that modifies a file
2. Check if GitHub automatically requests reviews from the users/teams you specified in CODEOWNERS

---

## ✅ Step 2: Configure Branch Protection

Branch protection enforces rules before a PR can be merged, including requiring approvals from CODEOWNERS.

### 2.1 Navigate to Branch Protection Settings

1. Go to your repository on GitHub
2. Click **"Settings"** (tab at the top)
3. Click **"Branches"** in the left sidebar
4. Click **"Add branch protection rule"** (or edit existing rule)

### 2.2 Create Protection Rule for `main`

1. In **"Branch name pattern"**, enter: `main`
2. Under **"Require a pull request before merging"**, check:
   - ✅ **Require a pull request before merging**
3. Under **"Require approvals"**, check:
   - ✅ **Require approvals**
   - Set **"Number of approvals required"** to `1` (or more)
4. **Critical:** Under **"Require review from Code Owners"**, check:
   - ✅ **Require review from Code Owners**
5. Under **"Restrict who can dismiss approvals"**, select:
   - **Only the latest reviewer** (recommended)
   - OR **Only certain people or teams** (if you have specific requirements)
6. Under **"Include administrators"**, check:
   - ✅ **Include administrators** (if you want admins to be subject to the same rules)
7. Under **"Require status checks to pass before merging"**, check:
   - ✅ **Require status checks to pass before merging** (optional but recommended)
8. Under **"Require branches to be up to date before merging"**, check:
   - ✅ **Require branches to be up to date before merging** (optional but recommended)
9. Under **"Require linear history"**, check:
   - ✅ **Require linear history** (optional, prevents merge commits)
10. Under **"Require deployment to succeed before merging"**, check:
    - ❌ Leave unchecked (unless you use GitHub Environments)
11. Under **"Require signed commits"**, check:
    - ❌ Leave unchecked (unless you use GPG-signed commits)
12. Under **"Block force pushes"**, check:
    - ✅ **Block force pushes** (recommended)
13. Under **"Block deletions"**, check:
    - ✅ **Block deletions** (recommended)

14. Click **"Create"** or **"Save changes"**

### 2.3 Verify Branch Protection is Working

1. Create a test pull request targeting `main`
2. Try to merge it without approvals - GitHub should block it
3. Get approval from a CODEOWNER - GitHub should now allow merging

---

## 🎯 Step 3: Test the Complete Workflow

### 3.1 Test CODEOWNERS Auto-Request

1. Create a new branch: `git checkout -b test-codeowners`
2. Modify a file that matches a CODEOWNERS rule (e.g., edit a file in `src/frontend/`)
3. Push and create a PR: `git push origin test-codeowners`
4. **Verify:** GitHub automatically requests review from the CODEOWNER(s) you specified

### 3.2 Test Branch Protection Enforcement

1. Have someone who is **NOT** a CODEOWNER approve the PR
2. Try to merge the PR
3. **Verify:** GitHub blocks the merge with a message like:
   > "This branch requires approval from @org/frontend-team"

4. Have a CODEOWNER approve the PR
5. **Verify:** The merge button is now enabled

---

## 👥 (Optional) Create GitHub Teams

If you're in a GitHub Organization, create teams to group reviewers:

### Create a Team

1. Go to your organization page on GitHub
2. Click **"Teams"** in the header
3. Click **"New team"**
4. Enter a **Team name** (e.g., `frontend-team`)
5. Select a **Visibility** (usually "Visible" or "Secret")
6. Click **"Create team"**
7. Click **"Add a member"** and add team members

### Use Teams in CODEOWNERS

In your `.github/CODEOWNERS` file, reference teams:

```plaintext
# All frontend code requires frontend-team approval
src/frontend/ @org/frontend-team

# All backend code requires backend-team approval  
src/backend/ @org/backend-team
```

### Use Teams in Branch Protection

1. Go to **Settings > Branches**
2. Edit your branch protection rule
3. Under **"Require approvals from specific people or teams"** (GitHub Enterprise/Pro only):
   - Add `@org/frontend-team`

---

## 🔧 Advanced Configuration

### Multiple CODEOWNERS Files

You can have CODEOWNERS files in multiple locations:
- `.github/CODEOWNERS` (applies to entire repo)
- `docs/CODEOWNERS` (applies to `/docs` directory)
- `CODEOWNERS` in root (applies to entire repo)

GitHub checks all locations and combines the rules.

### CODEOWNERS Syntax Reference

| Pattern | Matches |
|---------|---------|
| `*` | All files |
| `*.js` | All JavaScript files |
| `/docs/*` | All files directly in `/docs` |
| `/docs/**` | All files in `/docs` and subdirectories |
| `README.md` | Only the README.md file |
| `/apps/**/*.ts` | All TypeScript files in `/apps` or subdirectories |

### CODEOWNERS Comments

You can add comments to CODEOWNERS (lines starting with `#`):

```plaintext
# This is a comment
* @alice @bob

# Frontend team owns all frontend code
/src/frontend/ @org/frontend-team
```

---

## 🛡️ Security Considerations

### 1. CODEOWNERS and Forks

- CODEOWNERS **do not apply** to users from forked repositories
- Only repository collaborators can be CODEOWNERS
- External contributors cannot be CODEOWNERS

### 2. Admin Bypass

- Repository administrators can **bypass** branch protection by default
- To prevent this, enable **"Include administrators"** in branch protection settings

### 3. Force Push Protection

- Always enable **"Block force pushes"** in branch protection
- This prevents accidental (or malicious) history rewriting

---

## 📊 Example Scenarios

### Scenario 1: Small Team (2-5 People)

**CODEOWNERS:**
```plaintext
# All code requires approval from Alice or Bob
* @alice @bob
```

**Branch Protection:**
- Require 1 approval
- Require review from Code Owners
- Block force pushes

### Scenario 2: Medium Team with Specializations

**CODEOWNERS:**
```plaintext
# Default: All code requires any maintainer
* @org/maintainers

# Security files require security team
/security/ @org/security-team
/secrets/ @org/security-team

# Infrastructure requires devops team
/terraform/ @org/devops-team
/k8s/ @org/devops-team
Dockerfile @org/devops-team
```

**Branch Protection:**
- Require 1 approval
- Require review from Code Owners
- Block force pushes
- Include administrators

### Scenario 3: Large Organization with Strict Controls

**CODEOWNERS:**
```plaintext
# All code requires maintainer approval
* @org/maintainers

# Frontend code requires frontend team
/src/frontend/ @org/frontend-team

# Backend code requires backend team
/src/backend/ @org/backend-team

# Database migrations require DBA team
/migrations/ @org/dba-team
```

**Branch Protection:**
- Require 2 approvals
- Require review from Code Owners
- Require status checks to pass
- Require branches to be up to date
- Block force pushes
- Block deletions
- Include administrators

---

## 🔄 Updating CODEOWNERS

### Add a New CODEOWNER

1. Edit `.github/CODEOWNERS`
2. Add the new user/team to the appropriate rule
3. Commit and push the changes
4. GitHub automatically applies the new rules to future PRs

### Remove a CODEOWNER

1. Edit `.github/CODEOWNERS`
2. Remove the user/team from the rules
3. Commit and push the changes
4. **Note:** Existing PRs still require approval from the removed CODEOWNER

---

## ❓ Troubleshooting

### CODEOWNERS Not Requesting Reviews

**Check:**
1. Is the CODEOWNERS file in the correct location? (`.github/CODEOWNERS`, `docs/CODEOWNERS`, or `CODEOWNERS`)
2. Are the patterns correct? Test with `*` first
3. Are the usernames/team names correct? (Use `@username` or `@org/team-name`)
4. Is the CODEOWNERS file committed to the default branch?

### Branch Protection Not Enforcing CODEOWNERS

**Check:**
1. Is **"Require review from Code Owners"** checked in branch protection?
2. Is the branch protection rule applied to the correct branch?
3. Are you testing with a PR that actually modifies files matching CODEOWNERS rules?

### "This branch requires approval from..." Message Persists

**Check:**
1. Has a CODEOWNER actually approved the PR? (Comments don't count as approvals)
2. Did the CODEOWNER's approval get dismissed? (Check PR timeline)
3. Is the CODEOWNER still a collaborator on the repository?

### Admins Can Still Merge Without Approvals

**Fix:**
1. Go to branch protection settings
2. Enable **"Include administrators"**
3. Save changes

---

## 📚 Additional Resources

- [GitHub CODEOWNERS Documentation](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners)
- [GitHub Branch Protection Documentation](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches)
- [CODEOWNERS Syntax Reference](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/code-owners-syntax)

---

## ✅ Checklist

Before going live, verify:

- [ ] `.github/CODEOWNERS` file exists with correct rules
- [ ] Branch protection is enabled for `main` (or your default branch)
- [ ] **"Require review from Code Owners"** is checked
- [ ] **"Block force pushes"** is checked
- [ ] **"Include administrators"** is checked (if you want admins to follow the same rules)
- [ ] Test PR was created and CODEOWNERS were auto-requested
- [ ] Test PR cannot be merged without CODEOWNER approval
- [ ] All CODEOWNER usernames/teams are correct and active

---

*Last updated: $(date)*
