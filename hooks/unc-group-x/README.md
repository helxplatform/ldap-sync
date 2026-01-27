# unc-group-x Hook Service

This service is a hook for LDAP synchronization. It listens on port 5001
and processes incoming LDAP entries. Depending on the entry type, it
transforms the DN and attributes, creates derived search specifications,
and outputs a JSON response containing:
  - transformed
  - derived
  - dependencies
  - bindings
  - reset (legacy, always false)

## Conversion Process Summary

- **ORDRD Group (Example1):**
  - The groupname is extracted from the DN.
  - The DN is updated to "cn={{ groupname }},ou=groups,dc=example,dc=org".
  - Member entries use DN templates such as
    "uid=$pidUidMap.<pid>,ou=users,dc=example,dc=org".
  - Dependencies mirror the templated member DNs.
  - Derived search is created with a filter combining all pid values.

- **UNC User (Example2):**
  - The DN is built using the uid: "uid={{ uid }},ou=users,dc=example,dc=org".
  - The content is transformed to include key attributes and uses the
    global baseGid for "gidNumber".
  - A shared posixGroup is emitted using baseGroup with memberUid set
    to the user's uid.
  - A derived search is created based on uidNumber.
  - Bindings include "pidUidMap.<pid>" set to the user's uid.

- **Posix Group (Example3):**
  - The DN is transformed to "cn={{ cn }},ou=groups,dc=example,dc=org".
  - The content is adjusted by filtering out extra object classes.
  - If a memberuid field exists, it is promoted out of the content.
  - No derived searches are generated.

## Template and Binding Rules

- Any string in transformed or dependencies may include $variables.
- Variable names use dot segments, including numeric segments.
- The main process waits for all variables to be bound and dependencies
  to be synced before writing a transformed entry.

## Customizing the Transformation Logic

- The transformation code is located in the functions:
  - `processORDRDGroup`
  - `processUNCUser`
  - `processPosixGroup`

- To change how a field is transformed or to add additional logic,
  modify the corresponding function and update the routing in
  `hookHandler`.

## Building and Running

1. **Swagger Documentation:**
   - Run `make docs` to generate/update the Swagger docs using:
     `swag init -g main.go`

2. **Docker Build:**
   - Run `make build REPO=your-repo` to build the Docker image.
   - The build uses Go 1.23 for compilation and an Ubuntu image to run.

3. **Docker Push:**
   - Run `make push REPO=your-repo` to push the image to your registry.

4. **Running Locally:**
   - Execute the binary:
     ```
     ./unc-group-x -baseGid=200 -baseGroup=users
     ```
   - Or run the Docker container:
     ```
     docker run -p 5001:5001 $(REPO)/unc-group-x:$(VERSION)
     ```

## Suggestions and Clarifications

- Review the input DN format in the examples and adjust the helper
  functions if your environment deviates from these patterns.
- Ensure that pidUidMap bindings in the UNC User handler match the
  template variables used by group handling.
- Validate LDAP filters as needed for your deployment.
- If processing new object types, add similar transformation functions
  and update the routing logic in `hookHandler`.

Each line in this document is kept below 80 characters. Modify as needed
for your deployment.
