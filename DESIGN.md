# Spending Tracking App
## Purpose
This web app should allow users to, by some combination of manual data imports and API integrations where available, import their financial transaction data (from bank accounts/credit cards, primarily), and store them in the application's database. From that point, the application allows the user to view their financial health by exploring the data in a variety of ways. It should also offer users the ability to filter data freely and build custom dashboards from that data to create the views that suit their exact needs.

## Data Ingest
Data should be able to be imported in a few ways
### API Integration
This is not to be built immediately, since there are no use-cases currently available for the user base, but if/when a financial institution (FI) that users want support for offers a personal-use API, the application should be able to store credentials and, on an hourly job basis, be able to pull fresh data from that institution, and add new data to the DB if it exists.

### File-based imports
This is the most common method and most important. Users should be able to upload files created by export tools from a given FI, and the application should:
- use column headings (most files are CSV or similar) and contents to suggest a mapping to a field the application supports
- allow the user to edit the mapping manually
- allow the user to select from a preset of saved mappings they have created (allowing for re-use). These notably should not be restricted to one-per-FI, since multiple product types could have different schemas on export, and they may change over time
If a user re-submits a file with more data from a later point-in-time, the application should gracefully de-duplicate the overlapping entries
  - The app should have pre-created presets for known schemas of popular Canadian FIs, and also some basic CSV+JSON schemas users can use to create their own imports from raw data if so needed

### Manual event creation
If a user cannot use file or API import, a button should exist in the import page to simply manually create an event, allowing the user to select a linked account and fill out the metadata for the transaction in a form interface to be added to the DB.

Once data is loaded for ingest (aside from manual event creation, as this should be part of the form's inputs), the application should run a categorisation pass to categorise transactions. This is one of the most important features, as most FIs won't do this for you. If they do, users should be able to drop the field on import to get the application to perform the action instead. The valid categories are defined in user settings on a per-user basis. The application should have the option to:
- allow users to provide a cloud LLM provider account's API credentials to use it for the categorisation
- default back to an LLM endpoint provided in the server's deployment environment variables, which the server would then use to perform the task
Low-confidence results should be flagged for the user in their notifications post-import for them to review and adjust as-needed.

## Data Management
All user data should be encrypted on a per-user basis, ensuring a DB leak alone cannot compromise user security, and insulating the application from bugs where a user might be able to retrieve data outside of their account, as encryption can be a second-layer safeguard in addition to application logic.

### User settings
- Language (default to English, no need to build translations, but leave the door open for that to happen in the future)
- Default currency (should be prompted for this during the sign-up flow)
- Email (used to send reports and account emails)
- Notification settings to send monthly, quarterly, and/or annual reports automatically to their email
- Transaction categories (provide sensible defaults, which **are** able to be toggled off, and allow users to add more as desired)

### Database
There should be a basic user table, though we should leverage Auth0's free tier to handle authentication (application is not meant to scale past those limits), so we can ideally keep this fairly lean

There should be an accounts table that links accounts to users, and defines fields such as account type (bank/cash account vs credit card), subtypes such as savings vs chequing or variable vs fixed mortgages, account currency, a default schema ID to apply for imports to that account, and anything else that's necessary

There should be a table to store schemas, as jsonb or similar

The transaction table should support (this is a minimal set of fields, more can+should be added as necessary):
- account types (bank account, mortgage acct, credit card, etc)
- account subtypes (savings vs chequing or variable vs fixed mortgages)
- value
- date (executed + posted should be available, since some accts will have both)
- account ID (foreign key w/ the accounts table)
- category
- description (populated by data sources)
- notes (meant for user free-form comments in-app)

A budget table should exist for user-created budgets, storing budgets as jsonb or similar, which contain spending targets per-category (since categories exist per-user, this makes sense). They should also contain:
- foreign key for user ID
- period (a field to incdicate if the budget is weekly, monthly, yearly, etc)

## Pages
- User settings should be accessible to change settings, of course
- The main page should default to a monthly summary (a user setting could exist to change the default time-scale of this page), with a breakdown of spending per-category, how it matches up with the user's budget, if applicable, and how that compares to income for the period
- There should be an "explore" page where users can freely filter data and visualise their cash flow using any of the fields available to them
- There should be dashboards page where visualisations can be saved and used without having to re-create filters and change the visual presentation each time
- A budgets page should exist for users to create budgets, and explore their adherence to them
- If the user has a mortgage, the mortgage tab should exist to show them their progress in building up equity, explore their amortisation schedule, and play with variables like interest rate, making extra payments, etc. to see how those changes will impact their payments, term, etc.

## Architecture
The application should be comprised of, at least:
- a frontend, able to be served behind a k8s gateway API route(s)
- a backend, written in go. microservices are encouraged where they may sense, but are not encouraged "for the sake of it"
- a DB statefulset to deploy the DB, likely postgres unless there's a compelling alternative
- a deployment serving `llama.cpp` to host the model for transaction classification
- all this packaged into a helm chart for k8s deployment, targeting k8s 1.37
- HPAs where they make sense, PDBs everywhere, and metrics exposed via openmetrics
- APM for newrelic wired into the frontend+backend code
