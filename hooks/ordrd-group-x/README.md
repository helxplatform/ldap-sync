# ordrd-group-x Hook Service

This service is a hook for LDAP synchronization. It listens on port 5001
and processes incoming LDAP entries. Depending on the entry type, it
transforms the DN and attributes, creates derived search specifications,
and outputs a JSON response containing three keys:
  - transformed
  - derived
  - reset

## Conversion Process Summary

- **ORDRD Group (Example1):**
  - The groupname is extracted from the DN.
  - The DN is updated to "cn={{ groupname }},ou=groups,dc=example,dc=org".
  - In the content, "cn" is set to the groupname and each member is
    replaced using the pid-uid mapping.
  - Derived search is created with a filter combining all pid values.
  - If any uid mapping is missing, "transformed" is set to null and
    "reset" is true.

- **UNC User (Example2):**
  - The DN is built using the uid: "uid={{ uid }},ou=users,dc=example,dc=org".
  - The content is transformed to include key attributes and uses the
    global baseGid for "gidNumber".
  - A derived search is created based on uidNumber.
  - The pidUidMap is updated with the current entry mapping.

- **Posix Group (Example3):**
  - The DN is transformed to "cn={{ cn }},ou=groups,dc=example,dc=org".
  - The content is adjusted by filtering out extra object classes.
  - If a memberuid field exists, it is promoted out of the content.
  - No derived searches are generated.

## Customizing the Transformation Logic

- The transformation code is located in the functions:
  - `processORDRDGroup`
  - `processUNCUser`
  - `processPosixGroup`

- To change how a field is transformed or to add additional logic
  (e.g. new object types), modify the corresponding function.

- For example, you may wish to implement new filtering or state
  management. Replace the sample transformation logic with your own
  custom code where indicated by comments.

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
     ./ordrd-group-x -baseGid=200
     ```
   - Or run the Docker container:
     ```
     docker run -p 5001:5001 $(REPO)/ordrd-group-x:$(VERSION)
     ```

## Suggestions and Clarifications

- Review the input DN format in the examples and adjust the helper
  functions if your environment deviates from these patterns.
- Ensure that the pidUidMap population in the UNC User handler is done
  prior to processing groups that depend on it.
- Validate that the LDAP filters are correct; additional error checks
  may be added as needed.
- If processing new object types, add similar transformation functions
  and update the routing logic in `hookHandler`.

Each line in this document is kept below 80 characters. Modify as needed
to suit your deployment.
