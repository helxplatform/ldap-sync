# Hook Service Generation Prompt

You are an experienced Go developer. Your task is to build a hook
service that integrates with an existing LDAP synchronization system.
The hook service must be implemented in Go using the Echo framework,
and include OpenAPI documentation via swaggo (ensure that the annotations
are valid and parsable by swaggo).

The hook service will be called by the main LDAP system whenever it
detects a new or changed LDAP entry. It should expose a POST endpoint at
`/hook` that accepts a JSON payload containing only two fields:

- **"dn"**: a string representing the distinguished name (DN) of the
  LDAP entry.
- **"content"**: a JSON object representing the LDAP attributes of the
  entry. For each attribute, if there is a single value, it should be
  stored as a string; if there are multiple values, they should be stored
  as an array.

The hook service must process the payload as follows:

1. **Transformation**: It must apply a specialized transformation to
   the received payload. This transformation logic is defined by the
   example(s) below variable expressions are indicated by {{}}:

   *(Note: You may decide that the example is or is not sufficient and ask
   clarifying questions if needed. Also, verify that the filter and DN are
   correctly constructed to catch any user errors.)*

2. **Object Type Inspection**: The hook should inspect the incoming payload
   to determine its object type. There may be more than one incoming object
   type, and different processing should occur depending on the content.
   If the payload does not match an expected type, the hook should handle it
   appropriately (e.g., log an error or skip processing). Clearly mark where
   the user may substitute their own handling logic for different types.

3. **New search Definitions**:  The hook should determine if the contents
   should create addional new searches and populate Derived and is also
   defined by the examples.

4. **Response**: After applying the transformation, the hook service should
   return a JSON response containing three keys:

   - **"transformed"**: This is the result of performing the transformation
     login on the contents
     
   - **"derived"**: An array of additional search specifications. Each
     element in "derived" must be a JSON object that describes a search
     specification with at least the following fields:
       - **"id"**: a unique search identifier.
       - **"filter"**: an LDAP filter string.
       - **"refresh"**: an integer specifying the refresh interval in
         seconds.
       - **"baseDN"**: a string specifying the base DN to use for the search.
       
   - **"reset"**: A boolean directive. If set to `true`, the driver must
     discard its stored internal search results so that on the next run the
     hook is called again and can update its state. This is used when the
     transformation is dependent on the results of further searches.

4. **Processing Summary**: The hook service should output a summary of the
   conversion (the "transformed" and "derived" elements, as well as the
   "reset" directive) for debugging purposes. This summary should also be
   included in a README file along with instructions for how to customize
   the transformation logic.

**Examples**:  There a 2 types of input to expect and are described by
Example1 and Example2

   ***Example1 (ORDRD Group)**

   ****Example1 Special Instructions****

   In addition to the procesing described above based on Example1
   input and Example1 output perform the following

   The hook code should maintain a pid to uid map that will be populated
   by the processing of UNC Users (see Example2).

   The special instructions only affect value of transformation and reset.

   If any uids can not be found in the map, then set transformed to null
   and reset to true.  If all pids can be found in the pidUidMap, then use
   the value found in the map in transformed and perform the transformation
   as illustrated in the example below. 

   ****Example1 Input****

    {
      "dn": "cn=unc:app:renci:ordrd-example,ou=Groups,dc=unc,dc=edu",
      "content": {
        "cn": "unc:app:renci:ordrd-example",
        "description": "ordrd-example, RENCI, Applications, UNC Chapel Hill",
        "isPublic": "Y",
        "member": [
          "pid=713272486,ou=people,dc=unc,dc=edu",
          "pid=709909262,ou=people,dc=unc,dc=edu",
          "pid=730294000,ou=people,dc=unc,dc=edu",
          "pid=700268159,ou=people,dc=unc,dc=edu",
          "pid=730383111,ou=people,dc=unc,dc=edu"
        ],
        "objectClass": [
          "groupOfNames",
          "UNCGroup"
        ],
        "owner": "ou=groups,dc=unc,dc=edu"
      }
    }

  ****Example1 Output****
   
   {
    "transformed": {
      "dn": "cn=ordrd-example,ou=groups,dc=example,dc=org",
      "content": {
        "cn": "ordrd-example",
        "member": [
          "uid={{ piduidMap["713272486"] }},ou=users,dc=example,dc=org",
          "uid={{ piduidMap["709909262"] }},ou=users,dc=example,dc=org",
          "uid={{ piduidMap["730294000"] }},ou=users,dc=example,dc=org",
          "uid={{ piduidMap["700268159"] }},ou=users,dc=example,dc=org",
          "uid={{ piduidMap["730383111"] }},ou=users,dc=example,dc=org"
        ],
        "objectClass": [
          "top",
          "groupOfNames"
        ]
      }
    },
    "derived": [{
      "id": "ordrd-members",
      "filter": "(|(pid=713272486)(pid=709909262)(pid=730294000)(pid=700268159)(pid=730383111))",
      "refresh": 10,
      "baseDN": "ou=people,dc=unc,dc=edu"
    }],
    "reset": false
  }

   *** Example2 (UNC User)***

   ****Example2 Special Instructions***

   In addition to the procesing described above based on the example
   input and output perform the following

   Extract pid and uid from content to populate the pidUidMap decribed above

   ****Example2 Input****

    {
      "dn": "pid=702390258,ou=people,dc=unc,dc=edu",
      "content": {
        "addressIsPublic": "Y",
        "c": "US",
        "cn": "Karamarie Fecho",
        "departmentNumber": "637100",
        "displayName": "Karamarie Fecho",
        "eduPersonAffiliation": [
          "member",
          "affiliate",
          "alum"
        ],
        "eduPersonEntitlement": "urn:mace:dir:entitlement:common-lib-terms",
        "eduPersonNickname": "Karamarie",
        "eduPersonPrincipalName": "kfecho@unc.edu",
        "emailIsPublic": "Y",
        "facsimileIsPublic": "Y",
        "facsimileTelephoneNumber": "(919) 967-3893",
        "gidNumber": "200",
        "givenName": "Karamarie",
        "homeDirectory": "/home/k/f/kfecho",
        "homeLocation": "cn=home-location,pid=702390258,ou=People,dc=unc,dc=edu",
        "isActive": "Y",
        "isPublic": "Y",
        "location": [
          "cn=local-location,pid=702390258,ou=People,dc=unc,dc=edu",
          "cn=alternate-work-location,pid=702390258,ou=People,dc=unc,dc=edu",
          "cn=primary-work-location,pid=702390258,ou=People,dc=unc,dc=edu"
        ],
        "loginShell": "/bin/ksh",
        "mail": "kfecho@email.unc.edu",
        "massemailallowed": "Y",
        "objectClass": [
          "posixAccount",
          "UNCPerson",
          "top",
          "UNCAccount",
          "UNCAffiliate",
          "inetOrgPerson",
          "organizationalPerson",
          "eduPerson"
        ],
        "ou": "Renaissance Computing Inst",
        "phoneIsPublic": "Y",
        "pid": "702390258",
        "postalAddress": "Chapel Hill $ Chapel Hill, NC  27516 $ USA",
        "postalCode": "27516",
        "sn": "Fecho",
        "st": "NC",
        "street": "Chapel Hill",
        "telephoneNumber": "(919) 616-2808",
        "title": "Research Collaborator",
        "uid": "kfecho",
        "uidNumber": "6651",
        "uncAccountExpired": "N",
        "uncAffiliateType": "Research Collaborator",
        "uncAssociation": "cn=association-0,pid=702390258,ou=People,dc=unc,dc=edu",
        "uncEmail": [
          "kfecho@email.unc.edu",
          "kfecho@renci.org"
        ],
        "uncPidHistory": "702390258",
        "uncPreferredSurname": "Fecho",
        "uncReverseDisplayName": "Fecho, Karamarie",
        "uncServiceTag": [
          "WWW",
          "EXPIRED",
          "KRB"
        ]
      }
    }

   ****Example2 Output****

    {
    "transformed": {
      "dn": "uid=kfecho,ou=users,dc=example,dc=org",
      "content": {
        "cn": "Karamarie Fecho",
        "displayName": "Karamarie Fecho",
        "gidNumber": "200",
        "givenName": "Karamarie",
        "homeDirectory": "/home/k/f/kfecho",
        "objectClass": [
          "top",
          "inetOrgPerson",
          "posixAccount",
          "helxUser"
        ],
        "ou": "users",
        "sn": "Fecho",
        "uid": "kfecho",
        "uidNumber": "6651",
      }
    },
    "derived": []
  }


Application Id

The application name is ordrd-group-x
The application listens to port 5001

Your output should include the following artifacts:

## A. Go Source Code

1. Implement the hook service using the Echo framework.
2. Set the port to the value specified above
3. Use swaggo annotations to document the `/hook` endpoint (ensure these
   annotations are valid and can be parsed by swaggo).
4. The endpoint should accept a POST payload with "dn" and "content", apply
   the transformation as specified, validate the correctness of the filter
   and DN to catch user errors, and decide what to do based on the content
   type.
5. Return a JSON response with "transformed" (set to null), "derived", and
   "reset" as described.
6. Clearly mark the location where the user may replace the sample
   transformation logic with their own code.

## B. Dockerfile

1. Use Go 1.23 in the build stage.
2. Use an Ubuntu base image for the final container.
3. Expose the port specified above
4. The binary name and entry point are the application name

## C. Makefile

1. Include targets for building and pushing the Docker image.
2. The main target should be named `build` and must depend on a `docs`
   target. It builds the docker image
3. The `docs` target should generate or update the Swagger docs using
   `swag init -g main.go`.
4. Accept parameters for the repository and tag.
5. The platform is linux/amd64

## D. README

1. Provide a summary of the conversion process (the "transformed" and
   "derived" elements, as well as the "reset" directive) for debugging
   purposes.
2. Include instructions on how to customize the transformation logic and
   how to add additional handling for different incoming object types.
3. Include any clarifying questions or suggestions for further customization.
4. Respect the 80 character per line limit

Generate the complete code (Go source, Dockerfile, Makefile, and README)
accordingly.
